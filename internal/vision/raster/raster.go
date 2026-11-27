// Package raster rasterizes PDF pages and crops asset regions through bounded
// os/exec subprocesses (pdfinfo + mutool), never CGO. Every subprocess runs
// under a context timeout inside a private temporary directory that is removed
// on success, failure, cancellation and timeout alike. A process-wide
// semaphore bounds concurrent subprocesses even when callers construct many
// rasterizers.
package raster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Defaults for the resource limits. Zero in Config selects the default; a
// negative value is rejected.
const (
	DefaultDPI            = 250
	DefaultMaxPages       = 64
	DefaultMaxPagePixels  = 40 << 20 // matches the renderer's per-asset pixel bound
	DefaultMaxTotalPixels = 400 << 20
	DefaultMaxPageBytes   = 32 << 20
	DefaultProcessTimeout = 60 * time.Second
	DefaultStderrLimit    = 64 << 10
	MaxDPI                = 600
	waitDelay             = 2 * time.Second
)

// processSem is the global CPU/subprocess bound. It is shared by every
// MuPDFRasterizer, so concurrent construction cannot bypass the configured
// limit. SetProcessLimit adjusts it before work starts.
var (
	processSemOnce sync.Once
	processSemMu   sync.Mutex
	processSem     chan struct{}
)

func semaphore() chan struct{} {
	processSemOnce.Do(func() {
		n := runtime.NumCPU()
		if n < 1 {
			n = 1
		}
		if n > 4 {
			n = 4
		}
		processSem = make(chan struct{}, n)
	})
	processSemMu.Lock()
	defer processSemMu.Unlock()
	return processSem
}

// SetProcessLimit replaces the global subprocess bound. It must be called
// before any rasterization starts (typically once at startup or from a test);
// calls while work is in flight are rejected.
func SetProcessLimit(n int) error {
	if n < 1 {
		return errors.New("raster: process limit must be positive")
	}
	semaphore()
	processSemMu.Lock()
	defer processSemMu.Unlock()
	if len(processSem) != 0 {
		return errors.New("raster: process limit cannot change while subprocesses are running")
	}
	processSem = make(chan struct{}, n)
	return nil
}

func acquire(ctx context.Context) (func(), error) {
	sem := semaphore()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CommandFactory creates one subprocess. Tests inject it to run fake or slow
// commands through the identical semaphore, timeout, capture and cleanup path.
type CommandFactory func(ctx context.Context, bin string, args []string) *exec.Cmd

// Config configures a MuPDFRasterizer.
type Config struct {
	// MutoolPath and PDFInfoPath name the executables. Defaults: "mutool",
	// "pdfinfo" found on PATH.
	MutoolPath  string
	PDFInfoPath string
	// DPI is the rasterization resolution. Default 250; hard ceiling MaxDPI.
	DPI int
	// MaxPages bounds the source page count; checked before any draw runs.
	MaxPages int
	// MaxPagePixels bounds one decoded page; MaxTotalPixels bounds the whole
	// document decode; MaxPageBytes bounds one output PNG file.
	MaxPagePixels  int
	MaxTotalPixels int
	MaxPageBytes   int64
	// ProcessTimeout bounds one subprocess invocation.
	ProcessTimeout time.Duration
	// StderrLimit caps captured output per command.
	StderrLimit int64
	// TempRoot is where per-call work directories are created.
	TempRoot string
	// CommandFactory overrides subprocess creation; nil uses exec with a
	// fixed minimal environment.
	CommandFactory CommandFactory
}

// PageRange selects pages to render, inclusive and 1-based. Zero values mean
// "all pages".
type PageRange struct {
	First int
	Last  int
}

// Page is one decoded high-resolution page image. It never carries a file
// path: the temporary PNG is decoded and removed before Render returns.
type Page struct {
	Number int
	DPI    int
	Image  image.Image
}

// Rasterizer probes page counts and renders pages.
type Rasterizer interface {
	PageCount(ctx context.Context, pdf []byte) (int, error)
	Render(ctx context.Context, pdf []byte, pages PageRange) ([]Page, error)
}

// MuPDFRasterizer implements Rasterizer with pdfinfo + mutool draw.
type MuPDFRasterizer struct {
	cfg     Config
	command CommandFactory
}

var _ Rasterizer = (*MuPDFRasterizer)(nil)

// NewMuPDFRasterizer validates the configuration and resolves executables.
func NewMuPDFRasterizer(cfg Config) (*MuPDFRasterizer, error) {
	if cfg.DPI < 0 || cfg.MaxPages < 0 || cfg.MaxPagePixels < 0 || cfg.MaxTotalPixels < 0 ||
		cfg.MaxPageBytes < 0 || cfg.ProcessTimeout < 0 || cfg.StderrLimit < 0 {
		return nil, errors.New("raster: config values must not be negative")
	}
	if cfg.DPI == 0 {
		cfg.DPI = DefaultDPI
	}
	if cfg.DPI > MaxDPI {
		return nil, fmt.Errorf("raster: DPI %d exceeds the hard ceiling %d", cfg.DPI, MaxDPI)
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.MaxPagePixels == 0 {
		cfg.MaxPagePixels = DefaultMaxPagePixels
	}
	if cfg.MaxTotalPixels == 0 {
		cfg.MaxTotalPixels = DefaultMaxTotalPixels
	}
	if cfg.MaxPageBytes == 0 {
		cfg.MaxPageBytes = DefaultMaxPageBytes
	}
	if cfg.ProcessTimeout == 0 {
		cfg.ProcessTimeout = DefaultProcessTimeout
	}
	if cfg.StderrLimit == 0 {
		cfg.StderrLimit = DefaultStderrLimit
	}
	if cfg.MutoolPath == "" {
		cfg.MutoolPath = "mutool"
	}
	if cfg.PDFInfoPath == "" {
		cfg.PDFInfoPath = "pdfinfo"
	}
	command := cfg.CommandFactory
	if command == nil {
		for _, bin := range []string{cfg.MutoolPath, cfg.PDFInfoPath} {
			if _, err := exec.LookPath(bin); err != nil {
				return nil, fmt.Errorf("raster: executable %q: %w", bin, err)
			}
		}
		command = func(ctx context.Context, bin string, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, bin, args...)
		}
	}
	return &MuPDFRasterizer{cfg: cfg, command: command}, nil
}

// childEnv is the fixed minimal environment for PDF tools: no proxy, no
// locale surprises, no inherited secrets.
func childEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=/nonexistent"}
}

// limitedBuffer captures at most limit bytes and reports truncation.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// run executes one subprocess under the global semaphore and a per-command
// timeout, in dir. It never builds a shell command from user input: bin and
// args are fixed program arguments and paths this package generated.
func (r *MuPDFRasterizer) run(ctx context.Context, dir, bin string, args []string) (string, error) {
	release, err := acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()
	cmd := r.command(cmdCtx, bin, args)
	cmd.Dir = dir
	cmd.Env = childEnv()
	var out limitedBuffer
	out.limit = int(r.cfg.StderrLimit)
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("raster: %s failed: %v: %s", filepath.Base(bin), err, out.buf.String())
	}
	return out.buf.String(), nil
}

// withTempDir creates a private work directory and guarantees its removal on
// every path, including panic, cancel and timeout.
func (r *MuPDFRasterizer) withTempDir(ctx context.Context, fn func(dir string) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(r.cfg.TempRoot, "examforge-raster-")
	if err != nil {
		return fmt.Errorf("raster: create work dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("raster: remove work dir: %w", rmErr)
		}
	}()
	return fn(dir)
}

// pagesLineRe anchors the unique pdfinfo page-count line.
var pagesLineRe = regexp.MustCompile(`(?m)^Pages:[ \t]+([0-9]+)[ \t]*$`)

// PageCount probes the page count through pdfinfo's anchored "Pages:" line.
// A missing, duplicated or non-positive count is an error; the count above
// MaxPages is rejected before any draw could run.
func (r *MuPDFRasterizer) PageCount(ctx context.Context, pdf []byte) (int, error) {
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return 0, errors.New("raster: source is not a PDF")
	}
	var count int
	err := r.withTempDir(ctx, func(dir string) error {
		src := filepath.Join(dir, "source.pdf")
		if err := os.WriteFile(src, pdf, 0o600); err != nil {
			return err
		}
		out, err := r.run(ctx, dir, r.cfg.PDFInfoPath, []string{src})
		if err != nil {
			return err
		}
		matches := pagesLineRe.FindAllStringSubmatch(out, -1)
		if len(matches) != 1 {
			return fmt.Errorf("raster: pdfinfo reported %d Pages lines; exactly one is required", len(matches))
		}
		n, err := strconv.Atoi(matches[0][1])
		if err != nil || n < 1 {
			return fmt.Errorf("raster: pdfinfo page count %q is not a positive integer", matches[0][1])
		}
		if n > r.cfg.MaxPages {
			return fmt.Errorf("raster: source has %d pages; limit is %d", n, r.cfg.MaxPages)
		}
		count = n
		return nil
	})
	return count, err
}

// Render rasterizes the requested pages at the configured DPI and decodes
// each output PNG under the pixel and byte bounds. Pages come back in
// ascending page order.
func (r *MuPDFRasterizer) Render(ctx context.Context, pdf []byte, pages PageRange) ([]Page, error) {
	count, err := r.PageCount(ctx, pdf)
	if err != nil {
		return nil, err
	}
	first, last := pages.First, pages.Last
	if first == 0 {
		first = 1
	}
	if last == 0 {
		last = count
	}
	if first < 1 || last > count || first > last {
		return nil, fmt.Errorf("raster: page range %d-%d is outside 1-%d", first, last, count)
	}
	var out []Page
	err = r.withTempDir(ctx, func(dir string) error {
		src := filepath.Join(dir, "source.pdf")
		if err := os.WriteFile(src, pdf, 0o600); err != nil {
			return err
		}
		pattern := filepath.Join(dir, "page-%d.png")
		args := []string{"draw", "-F", "png", "-r", strconv.Itoa(r.cfg.DPI), "-o", pattern, src,
			fmt.Sprintf("%d-%d", first, last)}
		if _, err := r.run(ctx, dir, r.cfg.MutoolPath, args); err != nil {
			return err
		}
		total := 0
		for n := first; n <= last; n++ {
			path := filepath.Join(dir, fmt.Sprintf("page-%d.png", n))
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("raster: mutool did not produce page %d: %w", n, err)
			}
			if info.Size() > r.cfg.MaxPageBytes {
				return fmt.Errorf("raster: page %d PNG is %d bytes; limit %d", n, info.Size(), r.cfg.MaxPageBytes)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("raster: decode page %d: %w", n, err)
			}
			b := img.Bounds()
			pixels := b.Dx() * b.Dy()
			if b.Dx() <= 0 || b.Dy() <= 0 || pixels > r.cfg.MaxPagePixels {
				return fmt.Errorf("raster: page %d is %dx%d pixels; limit %d", n, b.Dx(), b.Dy(), r.cfg.MaxPagePixels)
			}
			total += pixels
			if total > r.cfg.MaxTotalPixels {
				return fmt.Errorf("raster: decoded pages exceed %d total pixels", r.cfg.MaxTotalPixels)
			}
			out = append(out, Page{Number: n, DPI: r.cfg.DPI, Image: img})
		}
		// No extra page files may exist: the output pattern must match the
		// requested range exactly.
		entries, err := filepath.Glob(filepath.Join(dir, "page-*.png"))
		if err != nil {
			return err
		}
		if len(entries) != last-first+1 {
			return fmt.Errorf("raster: mutool produced %d page files for %d requested pages", len(entries), last-first+1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
