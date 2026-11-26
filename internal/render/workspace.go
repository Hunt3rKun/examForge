package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"examforge/internal/ir"
)

// Render implements Renderer. It never mutates exam or layout.
func (r *TypstRenderer) Render(ctx context.Context, exam ir.ExamDocumentIR, layout ir.LayoutSpec) (*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, &RenderError{Op: "render", Err: err}
	}
	// A caller deadline is obeyed as is. Without one, bound the whole render,
	// including waiting for a slot: a queue wait must never be unbounded.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, r.cfg.Timeout, ErrRenderTimeout)
		defer cancel()
	}
	examData, err := json.Marshal(exam)
	if err != nil {
		return nil, &RenderError{Op: "validate exam", Err: fmt.Errorf("%w: encode: %v", ErrInvalidInput, err)}
	}
	layoutData, err := json.Marshal(layout)
	if err != nil {
		return nil, &RenderError{Op: "validate layout", Err: fmt.Errorf("%w: encode: %v", ErrInvalidInput, err)}
	}
	if err := r.validator.ValidateExam(examData); err != nil {
		return nil, &RenderError{Op: "validate exam", Err: fmt.Errorf("%w: %v", ErrInvalidInput, err)}
	}
	if err := r.validator.ValidateLayout(layoutData); err != nil {
		return nil, &RenderError{Op: "validate layout", Err: fmt.Errorf("%w: %v", ErrInvalidInput, err)}
	}
	payload, err := Precompile(exam, layout)
	if err != nil {
		return nil, err
	}
	// Wait for a slot. A context that ends while waiting returns at once: no
	// subprocess is started and no workspace is created.
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		if errors.Is(context.Cause(ctx), ErrRenderTimeout) {
			return nil, &RenderError{Op: "wait for render slot", Err: fmt.Errorf("%w: %w: %w", ErrQueueTimeout, ErrRenderTimeout, ctx.Err())}
		}
		return nil, &RenderError{Op: "wait for render slot", Err: ctx.Err()}
	}
	defer func() { <-r.sem }()

	ws, cleanup, err := r.assemble(ctx, exam, payload)
	if err != nil {
		return nil, err
	}
	artifact, err := r.compile(ctx, ws, payload)
	// Success, compile failure, cancellation: the workspace is always removed,
	// and its removal is reported when it fails.
	if cerr := cleanup(); cerr != nil && err == nil {
		err = cerr
	}
	return artifact, err
}

// assemble creates a private workspace and fills it. On success the caller
// must call the returned cleanup; on failure the workspace is already gone.
func (r *TypstRenderer) assemble(ctx context.Context, exam ir.ExamDocumentIR, payload *RenderPayload) (string, func() error, error) {
	work, err := os.MkdirTemp(r.cfg.TempRoot, "examforge-")
	if err != nil {
		return "", nil, &RenderError{Op: "create workspace", Err: pathOnly(err)}
	}
	cleanup := func() error {
		if rerr := os.RemoveAll(work); rerr != nil {
			return &RenderError{Op: "remove workspace", Err: fmt.Errorf("temporary files may remain: %v", pathOnly(rerr))}
		}
		return nil
	}
	if err := r.stage(ctx, work, exam, payload); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	return work, cleanup, nil
}

// stage fills one workspace: template, vendored package, assets, the option
// grids and the render payload. Nothing outside the workspace is written.
func (r *TypstRenderer) stage(ctx context.Context, ws string, exam ir.ExamDocumentIR, payload *RenderPayload) error {
	for name, body := range r.template {
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(name)), []byte(body), 0o400); err != nil {
			return &RenderError{Op: "stage template", Err: pathOnly(err)}
		}
	}
	if err := stagePackage(r.cfg.PackageDir, filepath.Join(ws, WorkspacePackages)); err != nil {
		return &RenderError{Op: "stage vendored packages", Err: err}
	}
	for _, dir := range []string{WorkspaceNoPackages, WorkspaceAssets} {
		if err := os.Mkdir(filepath.Join(ws, dir), 0o700); err != nil {
			return &RenderError{Op: "stage workspace", Err: pathOnly(err)}
		}
	}
	if err := r.copyAssets(ctx, ws, exam, payload); err != nil {
		return err
	}
	if err := r.resolveOptionGrids(ctx, ws, payload); err != nil {
		return err
	}
	payload.Pass = PassRender
	return r.writePayload(ws, payload)
}

// copyAssets resolves and copies every referenced asset once. Copies run in a
// bounded group and stop at the first failure or cancellation.
func (r *TypstRenderer) copyAssets(ctx context.Context, ws string, exam ir.ExamDocumentIR, payload *RenderPayload) error {
	if len(payload.Assets) == 0 {
		return nil
	}
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(r.cfg.AssetConcurrency)
	var total atomic.Int64
	for i := range payload.Assets {
		asset := &payload.Assets[i]
		group.Go(func() error {
			sum, size, err := r.copyAsset(gctx, ws, asset, exam.Assets[asset.AssetID])
			if err != nil {
				return fmt.Errorf("asset %q: %w", asset.AssetID, err)
			}
			if got := total.Add(size); got > r.cfg.MaxTotalAssetBytes {
				return fmt.Errorf("asset %q: %w: total asset bytes %d exceed limit %d", asset.AssetID, ErrAssetTooLarge, got, r.cfg.MaxTotalAssetBytes)
			}
			asset.SHA256 = sum
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		// Cancellation, a caller deadline or the renderer timeout end the
		// render; any other copy failure is an asset error.
		if cerr := r.contextFailure(ctx, ws, "copy assets", nil); cerr != nil {
			return cerr
		}
		return &RenderError{Op: "copy assets", Err: err}
	}
	return nil
}

// copyAsset streams one asset into the workspace and returns its SHA-256.
func (r *TypstRenderer) copyAsset(ctx context.Context, ws string, asset *ResolvedAsset, meta ir.Asset) (string, int64, error) {
	target := filepath.Join(ws, filepath.FromSlash(asset.Path))
	if _, err := relUnder(ws, target); err != nil {
		// Generated paths are always inside the workspace; never trust otherwise.
		return "", 0, fmt.Errorf("%w: target is outside the workspace", ErrAsset)
	}
	src, err := r.resolver.Resolve(ctx, asset.AssetID, meta)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrAsset, err)
	}
	defer src.Close()
	data, err := readAsset(ctx, src, r.cfg.MaxAssetBytes)
	if err != nil {
		return "", 0, err
	}
	if err := validateImageContent(asset.MediaType, data, r.cfg.MaxAssetPixels); err != nil {
		return "", 0, err
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return "", 0, fmt.Errorf("%w: create asset file in workspace: %v", ErrAsset, pathOnly(err))
	}
	_, writeErr := dst.Write(data)
	if writeErr == nil {
		writeErr = dst.Sync()
	}
	if cerr := dst.Close(); cerr != nil && writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		_ = os.Remove(target)
		return "", 0, fmt.Errorf("%w: write asset file in workspace: %v", ErrAsset, pathOnly(writeErr))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// readAsset reads one asset with a hard byte limit while honoring ctx.
func readAsset(ctx context.Context, src io.Reader, limit int64) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: read asset: %w", ErrAsset, err)
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return nil, fmt.Errorf("%w: limit is %d bytes", ErrAssetTooLarge, limit)
			}
			out.Write(buf[:n])
		}
		if errors.Is(rerr, io.EOF) {
			return out.Bytes(), nil
		}
		if rerr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, fmt.Errorf("%w: read asset: %w", ErrAsset, cerr)
			}
			return nil, fmt.Errorf("%w: read asset: %v", ErrAsset, rerr)
		}
	}
}

// optionMeasure is one option group the template measured in the measure pass.
// Wmax is the widest option and avail the available option width, in points.
// A nil Wmax means the group could not be measured.
type optionMeasure struct {
	Kind  string   `json:"kind"`
	Group int      `json:"group"`
	Wmax  *float64 `json:"wmax"`
	Avail *float64 `json:"avail"`
}

// resolveOptionGrids runs the measure pass for option groups whose grid is
// still "auto" and applies the uniform ladder to the measured widths. Explicit
// grids are kept as requested.
func (r *TypstRenderer) resolveOptionGrids(ctx context.Context, ws string, payload *RenderPayload) error {
	auto := make(map[int]*RenderQuestion)
	eachQuestion(payload, func(q *RenderQuestion) {
		if q.OptionRequested == OptionAuto && len(q.Options) > 0 {
			auto[q.Index] = q
		}
	})
	if len(auto) == 0 {
		return nil
	}
	payload.Pass = PassMeasure
	if err := r.writePayload(ws, payload); err != nil {
		return err
	}
	args := r.typstArgs("eval", "--format", "json", "--in", TemplateEntry, measureExpression)
	stdout, _, err := r.run(ctx, ws, "typst eval (measure options)", args, DefaultMeasureLimit)
	if err != nil {
		if ctx.Err() != nil {
			// Cancellation and deadline are never degraded into a fallback.
			return err
		}
		r.fallbackOptionGrids(payload, auto, "option_measure_failed",
			"measuring option widths failed; every auto option grid fell back to 4x1: "+lastLine(err))
		return nil
	}
	var measured []optionMeasure
	if uerr := json.Unmarshal(stdout, &measured); uerr != nil {
		r.fallbackOptionGrids(payload, auto, "option_measure_failed",
			"measuring option widths returned unreadable output; every auto option grid fell back to 4x1")
		return nil
	}
	gutter := mmToPt(payload.Layout.OptionGutterMM)
	for _, m := range measured {
		q, ok := auto[m.Group]
		if !ok || m.Kind != "option_width" || m.Wmax == nil || m.Avail == nil {
			continue
		}
		q.OptionGrid = chooseOptionGrid(OptionAuto, *m.Wmax, *m.Avail, gutter)
		q.OptionColumns = GridColumns(q.OptionGrid)
		delete(auto, m.Group)
	}
	for _, q := range auto {
		// Unmeasurable content is unbounded: one option per row.
		q.OptionGrid = Grid4x1
		q.OptionColumns = GridColumns(Grid4x1)
		payload.Warnings = append(payload.Warnings, RenderWarning{
			Code:    "option_unmeasured",
			Path:    q.Path,
			Message: "option widths could not be measured; the grid fell back to 4x1",
		})
	}
	return nil
}

// fallbackOptionGrids puts every still-auto group on one option per row and
// records why, so a measure failure is visible and never silently changes layout.
func (r *TypstRenderer) fallbackOptionGrids(payload *RenderPayload, auto map[int]*RenderQuestion, code, message string) {
	for _, q := range auto {
		q.OptionGrid = Grid4x1
		q.OptionColumns = GridColumns(Grid4x1)
	}
	payload.Warnings = append(payload.Warnings, RenderWarning{Code: code, Path: "exam", Message: message})
}

// writePayload replaces payload.json atomically. The measure and render
// passes each write it once, so the earlier read-only copy must be replaceable.
func (r *TypstRenderer) writePayload(ws string, payload *RenderPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return &RenderError{Op: "encode payload", Err: fmt.Errorf("%w: %v", ErrInvalidInput, err)}
	}
	tmp := filepath.Join(ws, PayloadFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o400); err != nil {
		return &RenderError{Op: "write payload", Err: pathOnly(err)}
	}
	if err := os.Rename(tmp, filepath.Join(ws, PayloadFile)); err != nil {
		return &RenderError{Op: "write payload", Err: pathOnly(err)}
	}
	return nil
}

// typstArgs are the flags of every Typst invocation. Typst only ever sees the
// workspace: it is the root, the only package cache and the only package path.
func (r *TypstRenderer) typstArgs(command string, args ...string) []string {
	return append([]string{
		command,
		"--root", ".",
		"--package-path", WorkspaceNoPackages,
		"--package-cache-path", WorkspacePackages,
		"--font-path", r.cfg.FontDir,
		"--ignore-system-fonts",
		"--creation-timestamp", creationTimestamp,
		"--jobs", "1",
		"--diagnostic-format", "short",
	}, args...)
}

// compile runs the render pass and reads the PDF back into memory, so the
// Artifact never depends on the temporary workspace.
func (r *TypstRenderer) compile(ctx context.Context, ws string, payload *RenderPayload) (*Artifact, error) {
	_, stderr, err := r.run(ctx, ws, "typst compile", r.typstArgs("compile", TemplateEntry, OutputFile), 0)
	warnings := append([]RenderWarning{}, payload.Warnings...)
	if err != nil {
		return nil, err
	}
	if text := scrub(ws, boundedText(stderr)); text != "" {
		warnings = append(warnings, RenderWarning{Code: "typst_diagnostic", Path: TemplateEntry, Message: text})
	}
	data, err := readPDF(filepath.Join(ws, OutputFile), DefaultOutputLimit)
	if err != nil {
		return nil, &RenderError{Op: "read PDF", Err: fmt.Errorf("%w: %v", ErrCompileFailed, err)}
	}
	return &Artifact{Bytes: data, MIMEType: MIMEPDF, Warnings: warnings}, nil
}

// eachQuestion visits questions and their children in pre-order.
func eachQuestion(payload *RenderPayload, fn func(*RenderQuestion)) {
	var walk func(qs []RenderQuestion)
	walk = func(qs []RenderQuestion) {
		for i := range qs {
			fn(&qs[i])
			walk(qs[i].Children)
		}
	}
	for i := range payload.Exam.Sections {
		walk(payload.Exam.Sections[i].Questions)
	}
}

// lastLine keeps the most specific line of an error for a warning message.
func lastLine(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	return lines[len(lines)-1]
}
