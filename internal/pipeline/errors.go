package pipeline

import (
	"errors"
	"fmt"

	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
)

// MaxErrorMessageRunes bounds the error text stored on ingest_jobs. A message
// is redacted before it is bounded.
const MaxErrorMessageRunes = 2000

// Stable codes for permanent failures.
const (
	CodeIRInvalid     = "ir_invalid"     // candidate IR failed schema or semantic validation
	CodeAssetInvalid  = "asset_invalid"  // asset bytes, type, size or declared digest mismatch
	CodeAssetConflict = "asset_conflict" // same asset ID or key, different content
	CodeSourceInvalid = "source_invalid" // source is not a PDF, oversize, or wrong digest
	CodeObjectInvalid = "object_invalid" // rejected key, permission or size in object storage
	CodeObjectMissing = "object_missing" // the registered source object does not exist
	CodeStoreInvalid  = "store_invalid"  // the store rejected the data
	CodeStoreConflict = "store_conflict" // the store found a conflicting record
	CodeMalformedArgs = "malformed_args" // the queued arguments cannot name a job
	CodeTransient     = "transient"
	// CodeParserPrefix precedes the parser's own code.
	CodeParserPrefix = "parser_"
)

// PermanentError is an ingest failure caused by the input. It is recorded on
// the job and the River job is cancelled, so it is never retried.
type PermanentError struct {
	Code string
	Err  error
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("permanent ingest failure (%s): %v", e.Code, e.Err)
}

func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is a PermanentError.
func IsPermanent(err error) bool {
	var e *PermanentError
	return errors.As(err, &e)
}

// Classify returns the stable code of err and whether it is permanent. An
// error with no recognizable class is transient: retrying is the safe
// default, and River's attempt limit bounds it.
func Classify(err error, fallback string) (code string, permanent bool) {
	if err == nil {
		return "", false
	}
	var permanentErr *PermanentError
	var parserErr *parser.Error
	switch {
	case errors.As(err, &permanentErr):
		return permanentErr.Code, true
	case errors.As(err, &parserErr) && parserErr.Permanent:
		return CodeParserPrefix + parserErr.Code, true
	case errors.Is(err, store.ErrConflict):
		return CodeStoreConflict, true
	case errors.Is(err, store.ErrInvalid):
		return CodeStoreInvalid, true
	case errors.Is(err, storage.ErrConflict):
		return CodeAssetConflict, true
	case errors.Is(err, storage.ErrNotFound):
		return CodeObjectMissing, true
	case errors.Is(err, storage.ErrTooLarge):
		return CodeObjectInvalid, true
	case errors.Is(err, storage.ErrInvalid), errors.Is(err, storage.ErrInvalidKey), errors.Is(err, storage.ErrForbidden):
		return CodeObjectInvalid, true
	}
	return fallback, false
}

// Permanent wraps a cause with a code.
func Permanent(code string, cause error) error { return &PermanentError{Code: code, Err: cause} }
