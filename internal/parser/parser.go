// Package parser is the boundary between a source PDF and a candidate Exam
// IR. A ParserAdapter reads the source bytes and returns the candidate
// document plus the asset bytes it references. It never writes a database,
// uploads objects or enqueues jobs: the ingest worker validates its output
// first and owns every side effect.
//
// No real VLM/OCR client exists in this slice; MockParserAdapter replays the
// golden fixture and exposes fault hooks for tests.
package parser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/storage"
)

// Source describes the uploaded source PDF and how to read it.
type Source struct {
	JobID      uuid.UUID
	StorageKey string
	// SHA256 is the digest registered for the source bytes at submission.
	SHA256 string
	// Open returns a stream over the source bytes. It must honor ctx and
	// bound the stream to MaxSourceBytes.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// AssetBlob is one image the parser extracted.
type AssetBlob struct {
	AssetID   string
	MediaType string
	Data      []byte
	// SHA256 is the digest the parser declares for Data. Empty means the
	// parser declares none; a declared value that does not match Data is a
	// permanent input failure.
	SHA256 string
}

// ParseResult is one parser output: a candidate IR and its asset bytes.
type ParseResult struct {
	Exam   ir.ExamDocumentIR
	Assets []AssetBlob
}

// ParserAdapter converts a source PDF into a candidate Exam IR.
type ParserAdapter interface {
	Parse(ctx context.Context, source Source) (ParseResult, error)
}

// Error classifies a parser failure. A permanent error is a property of the
// input and must not be retried; anything else may succeed on another attempt.
type Error struct {
	// Permanent marks input failures: unreadable or unsupported source,
	// declared data that does not match the bytes.
	Permanent bool
	Code      string
	Err       error
}

func (e *Error) Error() string {
	kind := "transient"
	if e.Permanent {
		kind = "permanent"
	}
	return fmt.Sprintf("parser %s error %s: %v", kind, e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Permanent wraps an input failure.
func Permanent(code string, err error) error { return &Error{Permanent: true, Code: code, Err: err} }

// Transient wraps a failure that may succeed on another attempt.
func Transient(code string, err error) error { return &Error{Code: code, Err: err} }

// IsPermanent reports whether err is a permanent parser failure.
func IsPermanent(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Permanent
}

// IsParser reports whether err came from a parser.
func IsParser(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// ErrNotPDF reports source bytes that are not a PDF.
var ErrNotPDF = errors.New("source is not a PDF")

// ErrHashMismatch reports bytes that differ from the registered digest.
var ErrHashMismatch = errors.New("source bytes do not match the registered SHA-256")

// ReadSource reads the source stream fully, bounded, and verifies the PDF
// signature and the registered digest. Every failure here is permanent.
func ReadSource(ctx context.Context, source Source) ([]byte, error) {
	if source.Open == nil {
		return nil, Permanent("source_unreadable", errors.New("source has no reader"))
	}
	rc, err := source.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, storage.MaxSourceBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Permanent("source_unreadable", err)
	}
	if len(data) > storage.MaxSourceBytes {
		return nil, Permanent("source_too_large", fmt.Errorf("source exceeds %d bytes", storage.MaxSourceBytes))
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, Permanent("not_a_pdf", ErrNotPDF)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != strings.ToLower(source.SHA256) {
		return nil, Permanent("source_hash_mismatch", ErrHashMismatch)
	}
	return data, nil
}
