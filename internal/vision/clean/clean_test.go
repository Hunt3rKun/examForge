package clean

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// synthFixture builds one PNG containing every category the acceptance matrix
// names, with an exact per-category pixel mask for removal/retention rates.
//
// Layout (400x200): white background; a light gray diagonal watermark band;
// red handwritten strokes (including near-0° hues); blue annotation strokes;
// pure black solid lines, thin 1px lines, a dashed line and dark "print"
// blocks.
type synthFixture struct {
	img   *image.RGBA
	masks map[string][]bool // category -> pixel membership, row-major
	w, h  int
}

func (f *synthFixture) idx(x, y int) int { return y*f.w + x }

func (f *synthFixture) paint(cat string, x, y int, c color.RGBA) {
	f.img.SetRGBA(x, y, c)
	f.masks[cat][f.idx(x, y)] = true
}

func newSynthFixture(t *testing.T) *synthFixture {
	t.Helper()
	f := &synthFixture{w: 400, h: 200, img: image.NewRGBA(image.Rect(0, 0, 400, 200)),
		masks: map[string][]bool{}}
	for _, cat := range []string{"red", "blue", "watermark", "black"} {
		f.masks[cat] = make([]bool, f.w*f.h)
	}
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for y := 0; y < f.h; y++ {
		for x := 0; x < f.w; x++ {
			f.img.SetRGBA(x, y, white)
		}
	}
	// Gray diagonal watermark band, brightness ~0.85, saturation 0.
	gray := color.RGBA{R: 217, G: 217, B: 217, A: 255}
	for y := 0; y < f.h; y++ {
		for x := 0; x < f.w; x++ {
			if d := x - 2*y; d >= 0 && d < 24 {
				f.paint("watermark", x, y, gray)
			}
		}
	}
	// Red strokes: pure red plus hues straddling 0° (350° and 8°).
	reds := []color.RGBA{
		{R: 230, G: 20, B: 20, A: 255}, // ~0°
		{R: 220, G: 30, B: 62, A: 255}, // ~350°
		{R: 225, G: 60, B: 30, A: 255}, // ~9°
	}
	for i, c := range reds {
		y0 := 20 + i*12
		for y := y0; y < y0+6; y++ {
			for x := 250; x < 380; x++ {
				f.paint("red", x, y, c)
			}
		}
	}
	// Blue strokes: ~210° and ~240°.
	blues := []color.RGBA{
		{R: 30, G: 90, B: 220, A: 255},
		{R: 40, G: 40, B: 210, A: 255},
	}
	for i, c := range blues {
		y0 := 80 + i*12
		for y := y0; y < y0+6; y++ {
			for x := 250; x < 380; x++ {
				f.paint("blue", x, y, c)
			}
		}
	}
	black := color.RGBA{A: 255}
	// Solid 4px line.
	for y := 150; y < 154; y++ {
		for x := 20; x < 380; x++ {
			f.paint("black", x, y, black)
		}
	}
	// Thin 1px line.
	for x := 20; x < 380; x++ {
		f.paint("black", x, 165, black)
	}
	// Dashed 2px line: 6 on, 6 off.
	for x := 20; x < 380; x++ {
		if (x/6)%2 == 0 {
			f.paint("black", x, 175, black)
			f.paint("black", x, 176, black)
		}
	}
	// Dark print blocks (simulated glyph strokes).
	for y := 185; y < 195; y++ {
		for x := 30; x < 200; x++ {
			if (x+y)%3 != 0 {
				f.paint("black", x, y, black)
			}
		}
	}
	return f
}

func (f *synthFixture) encode(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, f.img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rates decodes the cleaned PNG and computes per-category white-out rate and
// black retention rate against the exact input masks.
func (f *synthFixture) rates(t *testing.T, cleaned []byte) (removed map[string]float64, blackKept float64, nonWhite int) {
	t.Helper()
	out, err := png.Decode(bytes.NewReader(cleaned))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	whited := map[string]int{}
	blackN, blackKeptN := 0, 0
	for y := 0; y < f.h; y++ {
		for x := 0; x < f.w; x++ {
			r, g, b, _ := out.At(x, y).RGBA()
			isWhite := r == 0xffff && g == 0xffff && b == 0xffff
			if !isWhite {
				nonWhite++
			}
			for cat, mask := range f.masks {
				if !mask[f.idx(x, y)] {
					continue
				}
				counts[cat]++
				if cat == "black" {
					blackN++
					if r < 0x2000 && g < 0x2000 && b < 0x2000 {
						blackKeptN++
					}
				} else if isWhite {
					whited[cat]++
				}
			}
		}
	}
	removed = map[string]float64{}
	for _, cat := range []string{"red", "blue", "watermark"} {
		removed[cat] = float64(whited[cat]) / float64(counts[cat])
	}
	return removed, float64(blackKeptN) / float64(blackN), nonWhite
}

// TestHSVPhysicalFalsification is the acceptance test: >=95% of red, blue and
// gray watermark pixels removed; >=99% of black print/line pixels retained;
// the output still contains the expected non-white content (no all-white
// "success").
func TestHSVPhysicalFalsification(t *testing.T) {
	f := newSynthFixture(t)
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Clean(t.Context(), bytes.NewReader(f.encode(t)))
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	removed, blackKept, nonWhite := f.rates(t, res.PNG)
	for cat, rate := range removed {
		if rate < 0.95 {
			t.Errorf("%s removal rate %.3f < 0.95", cat, rate)
		}
	}
	if blackKept < 0.99 {
		t.Errorf("black retention rate %.3f < 0.99", blackKept)
	}
	if nonWhite == 0 {
		t.Error("cleaned image is entirely white; the black content was destroyed")
	}
	if res.SHA256 == "" || len(res.SHA256) != 64 {
		t.Errorf("bad digest %q", res.SHA256)
	}
}

func TestHueWrapAroundZero(t *testing.T) {
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// 350° and 8° must both be inside the wrapping red range.
	for _, rgb := range [][3]uint8{{220, 30, 62}, {225, 60, 30}} {
		h, s, v := rgbToHSV(rgb[0], rgb[1], rgb[2])
		if !c.classify(h, s, v) {
			t.Errorf("hue %.1f (sat %.2f val %.2f) not classified as red", h, s, v)
		}
	}
	// Green stays.
	h, s, v := rgbToHSV(30, 200, 30)
	if c.classify(h, s, v) {
		t.Error("green classified for removal")
	}
}

func TestThresholdBoundaries(t *testing.T) {
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	// Exactly at the dark protection ceiling: protected.
	if c.classify(0, 1, cfg.DarkProtectValueMax) {
		t.Error("pixel at dark-protect ceiling was removed")
	}
	// Just above dark protection, saturated red: removed.
	if !c.classify(0, 1, cfg.DarkProtectValueMax+0.01) {
		t.Error("bright red just above protection kept")
	}
	// Watermark floor: at the floor removed, below kept.
	if !c.classify(0, 0, cfg.WatermarkValueMin) {
		t.Error("gray at watermark floor kept")
	}
	if c.classify(0, 0, cfg.WatermarkValueMin-0.01) {
		t.Error("gray below watermark floor removed")
	}
	// Mid-gray (between protect ceiling and watermark floor) is kept.
	if c.classify(0, 0, 0.5) {
		t.Error("mid-gray removed")
	}
}

func TestTransparentInputBecomesWhite(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.SetRGBA(1, 1, color.RGBA{R: 255, A: 128}) // semi-transparent red over white
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Clean(t.Context(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			r, g, b, a := out.At(x, y).RGBA()
			if r != 0xffff || g != 0xffff || b != 0xffff || a != 0xffff {
				t.Fatalf("pixel (%d,%d) = %d %d %d %d, want opaque white", x, y, r, g, b, a)
			}
		}
	}
}

func TestCleanRejectsBadInput(t *testing.T) {
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Clean(t.Context(), bytes.NewReader([]byte("not an image"))); err == nil {
		t.Error("corrupt image accepted")
	}
	small := DefaultConfig()
	small.MaxInputBytes = 8
	sc, err := NewHSVCleaner(small)
	if err != nil {
		t.Fatal(err)
	}
	f := newSynthFixture(t)
	if _, err := sc.Clean(t.Context(), bytes.NewReader(f.encode(t))); err == nil {
		t.Error("oversize input accepted")
	}
	pixels := DefaultConfig()
	pixels.MaxPixels = 10
	pc, err := NewHSVCleaner(pixels)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Clean(t.Context(), bytes.NewReader(f.encode(t))); err == nil {
		t.Error("over-pixel input accepted")
	}
}

func TestCleanHonorsCancellation(t *testing.T) {
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	f := newSynthFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Clean(ctx, bytes.NewReader(f.encode(t))); err == nil {
		t.Error("cancelled context accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []func(*HSVConfig){
		func(c *HSVConfig) { c.RedHueLow = math.NaN() },
		func(c *HSVConfig) { c.SaturationMin = math.Inf(1) },
		func(c *HSVConfig) { c.RedHueLow = 360 },
		func(c *HSVConfig) { c.WatermarkValueMin = 1.5 },
		func(c *HSVConfig) { c.DarkProtectValueMax = 0.9 }, // above watermark floor
		func(c *HSVConfig) { c.WatermarkSaturationMax = 0.9 },
		func(c *HSVConfig) { c.MaxPixels = 0 },
	}
	for i, mutate := range bad {
		cfg := DefaultConfig()
		mutate(&cfg)
		if _, err := NewHSVCleaner(cfg); err == nil {
			t.Errorf("case %d: invalid config accepted", i)
		}
	}
}

func TestCleanWarnsWhenNothingRemoved(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	c, err := NewHSVCleaner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Clean(t.Context(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Error("no warning for a zero-removal clean")
	}
}
