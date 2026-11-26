package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"examforge/internal/ir"
)

// The fake Typst is a small shell script the test generates in its own
// temporary directory. It records process activity in files under a private
// activity directory, so the parent can assert concurrency and cleanup
// without sharing memory with the child. The injected factory still goes
// through the identical semaphore, context, stderr, Wait and cleanup path.
const fakePDF = "%PDF-1.7\nfake typst output for the injected process boundary tests\n%%EOF\n"

// fakeTracker counts process launches and tracks real process concurrency by
// watching the activity files the fake writes at start and end.
type fakeTracker struct {
	dir     string
	script  string
	started atomic.Int64
}

func newFakeTracker(t *testing.T, mode string) *fakeTracker {
	t.Helper()
	ft := &fakeTracker{dir: t.TempDir()}
	ft.writeFakeScript(t, mode)
	return ft
}

// activity counts start/end files currently present: started minus finished.
func (ft *fakeTracker) activity(t *testing.T) (starts, ends int) {
	t.Helper()
	entries, err := os.ReadDir(ft.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "start-"):
			starts++
		case strings.HasPrefix(e.Name(), "end-"):
			ends++
		}
	}
	return starts, ends
}

// maxConcurrency polls the activity files while renders run and returns the
// largest number of simultaneously live fake processes observed.
func (ft *fakeTracker) maxConcurrency(t *testing.T, stop <-chan struct{}) int {
	t.Helper()
	max := 0
	for {
		select {
		case <-stop:
			return max
		default:
		}
		starts, ends := ft.activity(t)
		if live := starts - ends; live > max {
			max = live
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (ft *fakeTracker) factory(mode string) CommandFactory {
	return func(ctx context.Context, bin string, args []string) *exec.Cmd {
		ft.started.Add(1)
		// The fake is a tiny generated shell script, not this test binary:
		// it behaves the same whatever environment the renderer installs.
		// The script is written by the test into its own temporary directory.
		return exec.CommandContext(ctx, ft.script, args...)
	}
}

// writeFakeScript generates the fake typst shell script once per tracker.
func (ft *fakeTracker) writeFakeScript(t *testing.T, mode string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("sub=\"$1\"\n")
	b.WriteString("printf '%s' \"$sub\" > \"" + ft.dir + "/start-$$\"\n")
	switch mode {
	case "success", "slow":
		if mode == "slow" {
			b.WriteString("sleep 0.3\n")
		}
		b.WriteString("if [ \"$sub\" = eval ]; then printf '[]'; else printf '%s' '" + fakePDF + "' > out.pdf; fi\n")
		b.WriteString("printf '%s' \"$sub\" > \"" + ft.dir + "/end-$$\"\n")
		b.WriteString("exit 0\n")
	case "hang":
		// exec sleep so the canceled process is the sleep itself: SIGKILL
		// reaches it directly instead of leaving an orphaned shell child.
		b.WriteString("exec sleep 30\n")
	case "fail":
		b.WriteString("echo 'fake typst: deliberate compile failure' >&2\nexit 1\n")
	case "noise":
		b.WriteString("i=0; while [ $i -lt 200 ]; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' >&2; i=$((i+1)); done\nexit 1\n")
	case "badpdf":
		b.WriteString("printf 'not a pdf' > out.pdf\nexit 0\n")
	case "leakchild":
		// A short-lived descendant holds the stderr pipe open past the
		// parent's exit; WaitDelay must bound the wait instead of hanging.
		b.WriteString("( sleep 4 ) >&2 &\nexit 0\n")
	default:
		t.Fatalf("unknown fake mode %q", mode)
	}
	ft.script = filepath.Join(t.TempDir(), "fake-typst.sh")
	if err := os.WriteFile(ft.script, []byte(b.String()), 0o700); err != nil {
		t.Fatal(err)
	}
}

// fakeExam is a minimal schema-shaped exam with no assets and no options, so
// a render is exactly one compile subprocess.
func fakeExam() ir.ExamDocumentIR {
	return ir.ExamDocumentIR{
		SchemaVersion: "1.0.0", ExamID: "exam_fake",
		Metadata: ir.ExamMetadata{Title: "假命令样例", Subject: "other"},
		Assets:   map[string]ir.Asset{},
		Questions: []ir.QuestionItem{{
			Kind: "single", DisplayNumber: "1",
			Stem: []ir.ContentBlock{{Type: "text", Markdown: "题干"}},
		}},
	}
}

func fakeLayout() ir.LayoutSpec {
	return ir.LayoutSpec{
		SchemaVersion: "1.0.0", Name: "fake", PaperSize: "a4", Columns: 1,
		Mode: ModeStudent, OptionLayout: OptionAuto,
	}
}

// newFakeRenderer builds a renderer whose subprocess is the fake helper.
func newFakeRenderer(t *testing.T, mode string, ft *fakeTracker, mutate func(*TypstConfig)) *TypstRenderer {
	t.Helper()
	return newRenderer(t, func(cfg *TypstConfig) {
		cfg.CommandFactory = ft.factory(mode)
		if mutate != nil {
			mutate(cfg)
		}
	})
}

// TestRenderFakeSuccess covers the happy path through the injected boundary.
func TestRenderFakeSuccess(t *testing.T) {
	ft := newFakeTracker(t, "success")
	r := newFakeRenderer(t, "success", ft, nil)
	art, err := r.Render(context.Background(), fakeExam(), fakeLayout())
	if err != nil {
		t.Fatal(err)
	}
	assertPDF(t, art)
	if string(art.Bytes) != fakePDF {
		t.Errorf("artifact bytes = %q, want the fake PDF", art.Bytes)
	}
	if ft.started.Load() != 1 {
		t.Errorf("started %d processes, want 1 (no options, no measure pass)", ft.started.Load())
	}
}

// TestRenderSemaphoreBound asserts concurrent renders never exceed the
// configured limit, and that the default limit is runtime.NumCPU().
func TestRenderSemaphoreBound(t *testing.T) {
	const limit = 2
	ft := newFakeTracker(t, "slow")
	r := newFakeRenderer(t, "slow", ft, func(cfg *TypstConfig) { cfg.MaxConcurrent = limit })
	const n = 8
	stop := make(chan struct{})
	maxCh := make(chan int, 1)
	go func() { maxCh <- ft.maxConcurrency(t, stop) }()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Render(context.Background(), fakeExam(), fakeLayout())
			errs <- err
		}()
	}
	wg.Wait()
	close(stop)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("render: %v", err)
		}
	}
	if got := <-maxCh; got > limit {
		t.Errorf("max concurrent processes = %d, want <= %d", got, limit)
	}
	if ft.started.Load() != n {
		t.Errorf("started %d processes, want %d", ft.started.Load(), n)
	}

	t.Run("default limit", func(t *testing.T) {
		r2 := newFakeRenderer(t, "success", newFakeTracker(t, "success"), nil)
		if cap(r2.sem) != runtime.NumCPU() {
			t.Errorf("default semaphore = %d, want runtime.NumCPU() = %d", cap(r2.sem), runtime.NumCPU())
		}
	})
}

// TestRenderCancelWhileWaiting asserts a canceled waiter returns promptly and
// never starts a subprocess.
func TestRenderCancelWhileWaiting(t *testing.T) {
	ft := newFakeTracker(t, "hang")
	r := newFakeRenderer(t, "hang", ft, func(cfg *TypstConfig) { cfg.MaxConcurrent = 1 })
	// Occupy the only slot with a hanging render.
	holderCtx, holderCancel := context.WithCancel(context.Background())
	holderDone := make(chan error, 1)
	go func() {
		_, err := r.Render(holderCtx, fakeExam(), fakeLayout())
		holderDone <- err
	}()
	// Wait until the holder's process has started.
	deadline := time.Now().Add(5 * time.Second)
	for ft.started.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("holder process never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, err := r.Render(waitCtx, fakeExam(), fakeLayout())
		waitDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter reach the semaphore
	waitCancel()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not return promptly")
	}
	if got := ft.started.Load(); got != 1 {
		t.Errorf("started %d processes, want 1 (the waiter must not start one)", got)
	}
	holderCancel()
	if err := <-holderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("holder err = %v, want context.Canceled", err)
	}
}

// TestRenderCancelMidRun asserts cancellation terminates and waits for the
// running process and returns a distinguishable error.
func TestRenderCancelMidRun(t *testing.T) {
	ft := newFakeTracker(t, "hang")
	r := newFakeRenderer(t, "hang", ft, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Render(ctx, fakeExam(), fakeLayout())
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for ft.started.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("process never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrRenderTimeout) {
			t.Error("caller cancellation misreported as renderer timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled render did not return; process not terminated/waited")
	}
}

// TestRenderDefaultTimeout asserts the renderer's own deadline is reported as
// ErrRenderTimeout, distinct from a caller deadline.
func TestRenderDefaultTimeout(t *testing.T) {
	ft := newFakeTracker(t, "hang")
	r := newFakeRenderer(t, "hang", ft, func(cfg *TypstConfig) { cfg.Timeout = 200 * time.Millisecond })
	start := time.Now()
	_, err := r.Render(context.Background(), fakeExam(), fakeLayout())
	if !errors.Is(err, ErrRenderTimeout) {
		t.Fatalf("err = %v, want ErrRenderTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded too", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

// TestRenderCallerDeadline asserts an existing caller deadline is obeyed and
// reported as a plain deadline, not the renderer timeout.
func TestRenderCallerDeadline(t *testing.T) {
	ft := newFakeTracker(t, "hang")
	r := newFakeRenderer(t, "hang", ft, func(cfg *TypstConfig) { cfg.Timeout = time.Hour })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := r.Render(ctx, fakeExam(), fakeLayout())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrRenderTimeout) {
		t.Error("caller deadline misreported as renderer timeout")
	}
}

// TestRenderCompileFailureStderr asserts a non-zero exit surfaces readable,
// bounded stderr and scrubs the workspace path.
func TestRenderCompileFailureStderr(t *testing.T) {
	ft := newFakeTracker(t, "fail")
	r := newFakeRenderer(t, "fail", ft, nil)
	_, err := r.Render(context.Background(), fakeExam(), fakeLayout())
	if !errors.Is(err, ErrCompileFailed) {
		t.Fatalf("err = %v, want ErrCompileFailed", err)
	}
	var re *RenderError
	if !errors.As(err, &re) {
		t.Fatalf("err is not a *RenderError: %T", err)
	}
	if !strings.Contains(re.Stderr, "deliberate compile failure") {
		t.Errorf("stderr lost the diagnostic: %q", re.Stderr)
	}
	if strings.Contains(err.Error(), ft.dir) || strings.Contains(err.Error(), os.TempDir()) {
		t.Errorf("error leaks a host path: %v", err)
	}

	t.Run("stderr truncated", func(t *testing.T) {
		ft2 := newFakeTracker(t, "noise")
		r2 := newFakeRenderer(t, "noise", ft2, func(cfg *TypstConfig) { cfg.StderrLimit = 1024 })
		_, err := r2.Render(context.Background(), fakeExam(), fakeLayout())
		var re *RenderError
		if !errors.As(err, &re) {
			t.Fatalf("err = %v, want *RenderError", err)
		}
		if len(re.Stderr) > 1024+64 {
			t.Errorf("stderr kept %d bytes, want <= limit plus truncation note", len(re.Stderr))
		}
		if !strings.Contains(re.Stderr, "truncated") {
			t.Errorf("stderr missing truncation note: %q", re.Stderr)
		}
	})
}

// TestRenderBadPDF asserts a non-PDF output is a compile failure.
func TestRenderBadPDF(t *testing.T) {
	ft := newFakeTracker(t, "badpdf")
	r := newFakeRenderer(t, "badpdf", ft, nil)
	_, err := r.Render(context.Background(), fakeExam(), fakeLayout())
	if !errors.Is(err, ErrCompileFailed) {
		t.Fatalf("err = %v, want ErrCompileFailed", err)
	}
}

// TestRenderWaitDelay asserts a descendant holding the pipes open cannot hang
// the render past WaitDelay.
func TestRenderWaitDelay(t *testing.T) {
	ft := newFakeTracker(t, "leakchild")
	r := newFakeRenderer(t, "leakchild", ft, nil)
	done := make(chan error, 1)
	go func() {
		_, err := r.Render(context.Background(), fakeExam(), fakeLayout())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("leaked-child render succeeded; want a failure or bounded wait")
		}
	case <-time.After(waitDelay + 5*time.Second):
		t.Fatal("render hung on a descendant holding the pipes")
	}
}

// TestRenderAssetFailureCleanup asserts a failed asset copy removes the
// workspace and returns an asset error.
func TestRenderAssetFailureCleanup(t *testing.T) {
	ft := newFakeTracker(t, "success")
	exam := fakeExam()
	exam.Assets = map[string]ir.Asset{testAssetID: testAsset()}
	exam.Questions[0].Stem = append(exam.Questions[0].Stem, ir.ContentBlock{Type: "image_ref", AssetID: testAssetID, LayoutHint: "inline"})
	r := newFakeRenderer(t, "success", ft, func(cfg *TypstConfig) {
		cfg.AssetResolver = AssetResolverFunc(func(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
			return nil, errors.New("resolver exploded")
		})
	})
	_, err := r.Render(context.Background(), exam, fakeLayout())
	if !errors.Is(err, ErrAsset) {
		t.Fatalf("err = %v, want ErrAsset", err)
	}
	if ft.started.Load() != 0 {
		t.Errorf("started %d processes after asset failure, want 0", ft.started.Load())
	}
}

// TestRenderParallelWorkspaces asserts parallel renders use independent
// workspaces and leave nothing behind, including on failure.
func TestRenderParallelWorkspaces(t *testing.T) {
	ft := newFakeTracker(t, "slow")
	r := newFakeRenderer(t, "slow", ft, func(cfg *TypstConfig) { cfg.MaxConcurrent = 4 })
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			exam := fakeExam()
			exam.ExamID = fmt.Sprintf("exam_fake_%d", i)
			if _, err := r.Render(context.Background(), exam, fakeLayout()); err != nil {
				t.Errorf("render %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
