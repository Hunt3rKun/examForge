package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// run executes one Typst command inside the workspace and always waits for
// the process, including after a cancellation or timeout. It returns stdout
// (only when stdoutLimit is positive) and bounded stderr. A failure is
// classified as cancellation, deadline or compile failure.
func (r *TypstRenderer) run(ctx context.Context, ws, op string, args []string, stdoutLimit int64) ([]byte, []byte, error) {
	cmd := r.command(ctx, r.cfg.TypstPath, args)
	cmd.Dir = ws
	cmd.Env = r.env
	cmd.Stdin = nil
	stderr := &limitedBuffer{limit: r.cfg.StderrLimit}
	cmd.Stderr = stderr
	var stdout *limitedBuffer
	if stdoutLimit > 0 {
		stdout = &limitedBuffer{limit: stdoutLimit}
		cmd.Stdout = stdout
	}
	// Never hang on a descendant that keeps the pipes open after the exit.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		if cerr := r.contextFailure(ctx, ws, op, stderr); cerr != nil {
			return nil, stderr.bytes(), cerr
		}
		return nil, stderr.bytes(), &RenderError{Op: op, Err: fmt.Errorf("%w: %v", ErrCompileFailed, scrub(ws, err.Error()))}
	}
	if err := cmd.Wait(); err != nil {
		if cerr := r.contextFailure(ctx, ws, op, stderr); cerr != nil {
			return nil, stderr.bytes(), cerr
		}
		return nil, stderr.bytes(), &RenderError{
			Op:     op,
			Err:    fmt.Errorf("%w: %v", ErrCompileFailed, scrub(ws, err.Error())),
			Stderr: stderr.text(ws),
		}
	}
	if stdout == nil {
		return nil, stderr.bytes(), nil
	}
	return stdout.bytes(), stderr.bytes(), nil
}

// contextFailure reports the ended context of an operation, or nil when the
// context is still live. A timeout this renderer added and a caller's
// deadline are told apart by their cause.
func (r *TypstRenderer) contextFailure(ctx context.Context, ws, op string, stderr *limitedBuffer) error {
	cerr := ctx.Err()
	if cerr == nil {
		return nil
	}
	var out string
	if stderr != nil {
		out = stderr.text(ws)
	}
	switch {
	case errors.Is(cerr, context.Canceled):
		return &RenderError{Op: op + " canceled", Err: context.Canceled, Stderr: out}
	case errors.Is(context.Cause(ctx), ErrRenderTimeout):
		return &RenderError{
			Op:     op,
			Err:    fmt.Errorf("%w after %s: %w", ErrRenderTimeout, r.cfg.Timeout, context.DeadlineExceeded),
			Stderr: out,
		}
	default:
		return &RenderError{Op: op, Err: context.DeadlineExceeded, Stderr: out}
	}
}

// limitedBuffer keeps the first limit bytes written to it and counts the rest,
// so a subprocess cannot grow memory without bound.
type limitedBuffer struct {
	mu      sync.Mutex
	limit   int64
	buf     bytes.Buffer
	dropped int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - int64(b.buf.Len()); room > 0 {
		take := min(room, int64(len(p)))
		b.buf.Write(p[:take])
		b.dropped += int64(len(p)) - take
	} else {
		b.dropped += int64(len(p))
	}
	return len(p), nil
}

func (b *limitedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// text returns the kept bytes with workspace paths scrubbed.
func (b *limitedBuffer) text(ws string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := scrub(ws, b.buf.String())
	if b.dropped > 0 {
		out += fmt.Sprintf("\n[... %d more bytes truncated]", b.dropped)
	}
	return strings.TrimSpace(out)
}

func boundedText(b []byte) string { return strings.TrimSpace(string(b)) }

// scrub replaces the workspace path so a host directory never reaches the user.
func scrub(ws, s string) string { return strings.ReplaceAll(s, ws, "<workspace>") }

// childEnv is the environment of every Typst subprocess. Typst package
// locations are removed: they are passed as explicit flags. Every proxy is
// blackholed so an unexpected package download fails closed, offline.
func childEnv() []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "TYPST_PACKAGE_PATH", "TYPST_PACKAGE_CACHE_PATH", "TYPST_ROOT":
			continue
		}
		env = append(env, kv)
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		env = append(env, name+"="+blackholeProxy)
	}
	return append(env, "NO_PROXY=", "no_proxy=")
}

// loadTemplateFiles reads the .typ files of the template directory once. The
// renderer keeps these immutable copies and writes them into every workspace.
func loadTemplateFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: read template directory: %v", ErrTemplate, pathOnly(err))
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".typ") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular file", ErrTemplate, name)
		}
		if len(files) >= maxTemplateFiles {
			return nil, fmt.Errorf("%w: template directory holds more than %d files", ErrTemplate, maxTemplateFiles)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrTemplate, name, pathOnly(err))
		}
		if info.Size() > maxTemplateFileBytes {
			return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTemplate, name, maxTemplateFileBytes)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrTemplate, name, pathOnly(err))
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("%w: %s is not valid UTF-8", ErrTemplate, name)
		}
		files[name] = string(data)
	}
	return files, nil
}

// readPDF reads the compiled output with a size limit and checks its header.
func readPDF(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, pathOnly(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, pathOnly(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("output is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("output exceeds %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, pathOnly(err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("output exceeds %d bytes", limit)
	}
	if !hasPDFHeader(data) {
		return nil, errors.New("output is not a PDF: missing %PDF- header")
	}
	return data, nil
}

// hasPDFHeader accepts a PDF header at the start, after whitespace or a BOM.
func hasPDFHeader(data []byte) bool {
	head := bytes.TrimLeft(data, " \t\r\n\xef\xbb\xbf")
	return bytes.HasPrefix(head, []byte("%PDF-"))
}
