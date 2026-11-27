// Package render converts a validated Exam IR and Layout Spec into a rendered
// Artifact. Render(ExamDocumentIR, LayoutSpec) -> Artifact is the only render
// entry point: layout policy never lives in command-line flags.
package render

import (
	"context"
	"errors"
	"fmt"
	"io"

	"examforge/internal/ir"
)

// MIMEType of the Typst PDF artifact.
const MIMEPDF = "application/pdf"

// Renderer renders one exam with one explicit layout. Implementations must not
// modify the exam or layout they are given.
type Renderer interface {
	Render(ctx context.Context, exam ir.ExamDocumentIR, layout ir.LayoutSpec) (*Artifact, error)
}

// Artifact is the output of one render. It is self-contained and never points
// at a temporary workspace.
type Artifact struct {
	Bytes    []byte
	MIMEType string
	Warnings []RenderWarning
}

// RenderWarning reports a non-fatal degradation: inline markup fallbacks,
// unknown blocks or layout fallbacks. It carries a path into the IR.
type RenderWarning struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Renderer error classes. Wrap one of these (or context.Canceled /
// context.DeadlineExceeded) so callers can classify failures with errors.Is.
var (
	ErrInvalidInput  = errors.New("invalid render input")
	ErrAsset         = errors.New("asset error")
	ErrAssetTooLarge = errors.New("asset exceeds size limit")
	ErrPackage       = errors.New("vendored typst package error")
	ErrTemplate      = errors.New("template error")
	ErrCompileFailed = errors.New("typst compile failed")
	// ErrRenderTimeout is the cause of a deadline that this renderer added
	// because the caller passed a context without one.
	ErrRenderTimeout = errors.New("render timeout")
	// ErrQueueTimeout reports that a render's bounded deadline expired while
	// waiting for a concurrency slot, before any subprocess started.
	ErrQueueTimeout = errors.New("render queue timeout")
)

// AssetResolver locates asset content by ID. Implementations must not trust the
// IR uri as a filesystem path; the renderer never opens a uri directly.
type AssetResolver interface {
	Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error)
}

// RenderScopedResolver creates an isolated asset snapshot/cache for one Render
// call. Renderers detect it so a resolver cannot retain selector state between
// separate documents.
type RenderScopedResolver interface{ ForRender() AssetResolver }

// RenderError is the error type every renderer failure wraps. Stderr is the
// bounded, workspace-scrubbed output of the failed process, if any.
type RenderError struct {
	Op     string // the failing step, e.g. "typst compile"
	Err    error  // the cause; never nil
	Stderr string
}

func (e *RenderError) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Op, e.Err)
	if e.Stderr != "" {
		msg += "\n" + e.Stderr
	}
	return msg
}

func (e *RenderError) Unwrap() error { return e.Err }
