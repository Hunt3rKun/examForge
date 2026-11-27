package raster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"examforge/internal/vision/testpdf"
)

// ── real mutool / pdfinfo physical tests ────────────────────────────────────

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"mutool", "pdfinfo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
}

func TestRealPageCountAndRender(t *testing.T) {
	requireTools(t)
	pdf := testpdf.Build(3, 200, 100, [][3]float64{{1, 0, 0}, {0, 0, 1}, {0, 1, 0}})
	r, err := NewMuPDFRasterizer(Config{DPI: 100})
	if err != nil {
		t.Fatal(err)
	}
	count, err := r.PageCount(t.Context(), pdf)
	if err != nil || count != 3 {
		t.Fatalf("PageCount = %d, %v; want 3", count, err)
	}
	pages, err := r.Render(t.Context(), pdf, PageRange{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("rendered %d pages, want 3", len(pages))
	}
	// 200x100 pt at 100 DPI is about 278x139 px; assert the DPI actually took
	// effect by checking against the 72-DPI default size.
	for i, p := range pages {
		if p.Number != i+1 || p.DPI != 100 {
			t.Errorf("page %d = number %d dpi %d", i, p.Number, p.DPI)
		}
		b := p.Image.Bounds()
		wantW := 200 * 100 / 72
		if b.Dx() < wantW-2 || b.Dx() > wantW+2 {
			t.Errorf("page %d width %d, want ~%d at 100 DPI", p.Number, b.Dx(), wantW)
		}
		// Each page is a solid color: verify the center pixel.
		cr, cg, cb, _ := p.Image.At(b.Dx()/2, b.Dy()/2).RGBA()
		colors := [][3]uint32{{0xffff, 0, 0}, {0, 0, 0xffff}, {0, 0xffff, 0}}
		want := colors[i]
		if cr != want[0] || cg != want[1] || cb != want[2] {
			t.Errorf("page %d center = (%d,%d,%d), want %v", p.Number, cr, cg, cb, want)
		}
	}
}

func TestRealPageRangeSubset(t *testing.T) {
	requireTools(t)
	pdf := testpdf.Build(3, 100, 100, nil)
	r, err := NewMuPDFRasterizer(Config{DPI: 72})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := r.Render(t.Context(), pdf, PageRange{First: 2, Last: 3})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(pages) != 2 || pages[0].Number != 2 || pages[1].Number != 3 {
		t.Fatalf("pages = %+v, want numbers 2 and 3", []int{pages[0].Number, pages[1].Number})
	}
}

func TestRealTooManyPagesRejectedBeforeDraw(t *testing.T) {
	requireTools(t)
	pdf := testpdf.Build(3, 100, 100, nil)
	var drawCalled atomic.Bool
	factory := func(ctx context.Context, bin string, args []string) *exec.Cmd {
		if len(args) > 0 && args[0] == "draw" {
			drawCalled.Store(true)
		}
		return exec.CommandContext(ctx, bin, args...)
	}
	r, err := NewMuPDFRasterizer(Config{MaxPages: 2, CommandFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), pdf, PageRange{}); err == nil {
		t.Fatal("Render accepted a 3-page source with MaxPages=2")
	}
	if drawCalled.Load() {
		t.Fatal("mutool draw ran although the page count check failed")
	}
}

func TestRealInvalidRangeAndNonPDF(t *testing.T) {
	requireTools(t)
	pdf := testpdf.Build(2, 100, 100, nil)
	r, err := NewMuPDFRasterizer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), pdf, PageRange{First: 2, Last: 5}); err == nil {
		t.Error("out-of-range page selection accepted")
	}
	if _, err := r.PageCount(t.Context(), []byte("not a pdf")); err == nil {
		t.Error("non-PDF accepted")
	}
}

func TestRealTempDirsCleanedOnEveryPath(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	pdf := testpdf.Build(1, 100, 100, nil)
	r, err := NewMuPDFRasterizer(Config{TempRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	// Success path.
	if _, err := r.Render(t.Context(), pdf, PageRange{}); err != nil {
		t.Fatal(err)
	}
	// Failure path (bad range).
	_, _ = r.Render(t.Context(), pdf, PageRange{First: 5})
	// Cancelled path.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = r.Render(ctx, pdf, PageRange{})
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("work dirs leaked: %v", entries)
	}
}

// ── injected fake command: seam, argv, timeout, cleanup ─────────────────────

// fakeTool writes a generated shell script that plays pdfinfo or mutool. It
// records argv per invocation and process activity in files, so tests assert
// the factory is honored and subprocesses stay bounded without sharing memory
// with the child.
type fakeTool struct {
	dir    string
	script string
	calls  atomic.Int64
}

func newFakeTool(t *testing.T, body string) *fakeTool {
	t.Helper()
	ft := &fakeTool{dir: t.TempDir()}
	ft.script = filepath.Join(ft.dir, "fake.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + filepath.Join(ft.dir, "argv") + "\"\n" + body
	if err := os.WriteFile(ft.script, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return ft
}

func (ft *fakeTool) factory() CommandFactory {
	return func(ctx context.Context, bin string, args []string) *exec.Cmd {
		ft.calls.Add(1)
		return exec.CommandContext(ctx, ft.script, args...)
	}
}

func (ft *fakeTool) argv(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ft.dir, "argv"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

const fakeInfoOK = `case "$1" in
  */source.pdf) printf 'Pages:           2\n' ;;
esac
`

func TestFakeFactoryIsUsedWithFixedArgs(t *testing.T) {
	root := t.TempDir()
	ft := newFakeTool(t, fakeInfoOK+`
if [ "$1" = draw ]; then
  # Emit one small valid PNG per requested page from the output pattern.
  out=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "-o" ]; then out="$a"; fi
    prev="$a"
  done
  pre="${out%%%d*}"
  post="${out##*%d}"
  printf '\211PNG\r\n\032\n' > "${pre}1${post}"
  cp "${pre}1${post}" "${pre}2${post}"
fi
exit 0
`)
	r, err := NewMuPDFRasterizer(Config{TempRoot: root, DPI: 250, CommandFactory: ft.factory()})
	if err != nil {
		t.Fatal(err)
	}
	// Decode fails (the fake PNG is only a signature) but the argv assertions
	// prove the seam and fixed argument list.
	_, err = r.Render(t.Context(), []byte("%PDF-1.4 fake"), PageRange{})
	if err == nil || !strings.Contains(err.Error(), "decode page 1") {
		t.Fatalf("expected decode failure after fake draw, got %v", err)
	}
	if ft.calls.Load() == 0 {
		t.Fatal("injected CommandFactory was never used")
	}
	log := ft.argv(t)
	var drawLine string
	for _, line := range log {
		if strings.HasPrefix(line, "draw ") {
			drawLine = line
		}
	}
	if drawLine == "" {
		t.Fatalf("no draw invocation in %v", log)
	}
	for _, want := range []string{"-F png", "-r 250", "-o ", "page-%d.png", "1-2"} {
		if !strings.Contains(drawLine, want) {
			t.Errorf("draw argv %q lacks %q", drawLine, want)
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("work dirs leaked: %v", entries)
	}
}

func TestFakePagesLineVariants(t *testing.T) {
	cases := map[string]string{
		"missing":   "printf 'Title: x\\n'\n",
		"duplicate": "printf 'Pages: 2\\nPages: 2\\n'\n",
		"zero":      "printf 'Pages: 0\\n'\n",
		"nonint":    "printf 'Pages: two\\n'\n",
		"anchored":  "printf 'xPages: 2\\n'\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ft := newFakeTool(t, body+"exit 0\n")
			r, err := NewMuPDFRasterizer(Config{CommandFactory: ft.factory()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.PageCount(t.Context(), []byte("%PDF-1.4 x")); err == nil {
				t.Fatalf("PageCount accepted %s Pages output", name)
			}
		})
	}
}

func TestFakeNonZeroExitIsStableError(t *testing.T) {
	ft := newFakeTool(t, "echo 'boom' >&2\nexit 3\n")
	r, err := NewMuPDFRasterizer(Config{CommandFactory: ft.factory()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.PageCount(t.Context(), []byte("%PDF-1.4 x"))
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want command failure, got %v", err)
	}
}

func TestFakeSlowCommandTimesOutAndCleansUp(t *testing.T) {
	root := t.TempDir()
	ft := newFakeTool(t, "exec sleep 30\n")
	r, err := NewMuPDFRasterizer(Config{TempRoot: root, ProcessTimeout: 150 * time.Millisecond, CommandFactory: ft.factory()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = r.PageCount(t.Context(), []byte("%PDF-1.4 x"))
	if err == nil {
		t.Fatal("slow command did not time out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("work dirs leaked after timeout: %v", entries)
	}
}

func TestFakeCancelledContextStopsBeforeLaunch(t *testing.T) {
	ft := newFakeTool(t, fakeInfoOK)
	r, err := NewMuPDFRasterizer(Config{CommandFactory: ft.factory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.PageCount(ctx, []byte("%PDF-1.4 x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if ft.calls.Load() != 0 {
		t.Fatal("subprocess launched for a cancelled context")
	}
}

func TestGlobalSemaphoreBoundsConcurrentProcesses(t *testing.T) {
	if err := SetProcessLimit(2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetProcessLimit(4) })
	dir := t.TempDir()
	// The fake sleeps while holding an activity marker; the parent samples
	// live markers to measure real subprocess concurrency.
	ft := newFakeTool(t, `touch "`+dir+`/live-$$"
sleep 0.15
rm -f "`+dir+`/live-$$"
printf 'Pages: 1\n'
exit 0
`)
	var maxLive atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			entries, _ := filepath.Glob(filepath.Join(dir, "live-*"))
			if n := int64(len(entries)); n > maxLive.Load() {
				maxLive.Store(n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Two rasterizers, one shared global bound.
			r, err := NewMuPDFRasterizer(Config{CommandFactory: ft.factory()})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := r.PageCount(t.Context(), []byte("%PDF-1.4 x")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if maxLive.Load() > 2 {
		t.Fatalf("observed %d concurrent subprocesses; global limit is 2", maxLive.Load())
	}
	if maxLive.Load() == 0 {
		t.Fatal("no subprocess activity observed")
	}
}

func TestStderrCaptureIsBounded(t *testing.T) {
	ft := newFakeTool(t, "i=0; while [ $i -lt 500 ]; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' >&2; i=$((i+1)); done\nexit 1\n")
	r, err := NewMuPDFRasterizer(Config{StderrLimit: 1024, CommandFactory: ft.factory()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.PageCount(t.Context(), []byte("%PDF-1.4 x"))
	if err == nil {
		t.Fatal("failing command accepted")
	}
	if len(err.Error()) > 2048 {
		t.Fatalf("error carries %d bytes of captured output", len(err.Error()))
	}
}

// ── CropPNG geometry ─────────────────────────────────────────────────────────

func TestCropPNGGeometry(t *testing.T) {
	page := solidImage(200, 100)
	res, err := CropPNG(page, [4]float64{0.25, 0.25, 0.75, 0.75}, 100, CropConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 100 || res.Height != 50 {
		t.Fatalf("crop = %dx%d, want 100x50", res.Width, res.Height)
	}
	if res.AspectRatio != 2 {
		t.Errorf("aspect = %v, want 2", res.AspectRatio)
	}
	// 100 px at 100 DPI = 1 inch = 25.4 mm.
	if res.SuggestedWidthMM != 25.4 {
		t.Errorf("suggested width = %v mm, want 25.4", res.SuggestedWidthMM)
	}
	img, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatalf("crop is not a PNG: %v", err)
	}
	if img.Bounds().Dx() != 100 || img.Bounds().Dy() != 50 {
		t.Errorf("decoded crop = %v", img.Bounds())
	}
	if res.SHA256 == "" || len(res.SHA256) != 64 {
		t.Errorf("bad digest %q", res.SHA256)
	}
}

func TestCropPNGOutwardRoundingAndPaddingClamp(t *testing.T) {
	page := solidImage(100, 100)
	// Fractional coordinates round outward: 0.333*100=33.3 -> 33;
	// 0.667*100=66.7 -> 67.
	res, err := CropPNG(page, [4]float64{0.333, 0.333, 0.667, 0.667}, 100, CropConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 34 || res.Height != 34 {
		t.Fatalf("crop = %dx%d, want outward-rounded 34x34", res.Width, res.Height)
	}
	// Padding expands, then clamps to the image bounds on all four sides.
	res, err = CropPNG(page, [4]float64{0, 0, 1, 1}, 100, CropConfig{PaddingMM: 25.4})
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 100 || res.Height != 100 {
		t.Fatalf("padded full-page crop = %dx%d, want clamped 100x100", res.Width, res.Height)
	}
	res, err = CropPNG(page, [4]float64{0.4, 0.4, 0.6, 0.6}, 100, CropConfig{PaddingMM: 2.54})
	if err != nil {
		t.Fatal(err)
	}
	// 2.54mm at 100dpi = 10px per side: 20+20 = 40.
	if res.Width != 40 || res.Height != 40 {
		t.Fatalf("padded crop = %dx%d, want 40x40", res.Width, res.Height)
	}
}

func TestCropPNGSuggestedWidthCapped(t *testing.T) {
	page := solidImage(2000, 100)
	res, err := CropPNG(page, [4]float64{0, 0, 1, 1}, 100, CropConfig{MaxContentWidthMM: 150})
	if err != nil {
		t.Fatal(err)
	}
	if res.SuggestedWidthMM != 150 {
		t.Fatalf("suggested width = %v, want cap 150", res.SuggestedWidthMM)
	}
}

func TestCropPNGRejectsInvalidInput(t *testing.T) {
	page := solidImage(100, 100)
	nan := nan()
	cases := map[string][4]float64{
		"x order":   {0.5, 0.1, 0.4, 0.9},
		"y order":   {0.1, 0.9, 0.9, 0.1},
		"zero area": {0.5, 0.5, 0.5, 0.9},
		"negative":  {-0.1, 0.1, 0.5, 0.5},
		"over one":  {0.1, 0.1, 1.5, 0.5},
		"nan":       {nan, 0.1, 0.5, 0.5},
		"inf":       {0.1, 0.1, inf(), 0.5},
	}
	for name, rect := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CropPNG(page, rect, 100, CropConfig{}); err == nil {
				t.Fatalf("CropPNG accepted %s", name)
			}
		})
	}
	if _, err := CropPNG(page, [4]float64{0.1, 0.1, 0.9, 0.9}, 100, CropConfig{MaxPixels: 10}); err == nil {
		t.Error("pixel limit not enforced")
	}
	if _, err := CropPNG(page, [4]float64{0.1, 0.1, 0.9, 0.9}, 0, CropConfig{}); err == nil {
		t.Error("zero DPI accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := NewMuPDFRasterizer(Config{DPI: MaxDPI + 1}); err == nil {
		t.Error("DPI above hard ceiling accepted")
	}
	if _, err := NewMuPDFRasterizer(Config{MaxPages: -1}); err == nil {
		t.Error("negative limit accepted")
	}
}

func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{A: 255})
		}
	}
	return img
}

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }

func TestMain(m *testing.M) {
	// Keep the global subprocess bound small and deterministic in tests.
	if err := SetProcessLimit(4); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
