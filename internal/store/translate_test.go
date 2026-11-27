package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"examforge/internal/store"
)

// TestTranslateClassifiesPostgresErrors pins the SQLSTATE mapping. A NOT NULL
// violation is caller data, not a transient outage: if it fell through
// untranslated the ingest worker would retry a defect forever. That exact bug
// was found only against a real database, so the mapping is asserted offline.
func TestTranslateClassifiesPostgresErrors(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		column string
		want   error
	}{
		{"unique violation", "23505", "", store.ErrConflict},
		{"foreign key violation", "23503", "", store.ErrConflict},
		{"not null violation", "23502", "knowledge_points", store.ErrInvalid},
		{"check violation", "23514", "", store.ErrInvalid},
		{"numeric overflow", "22003", "", store.ErrInvalid},
		{"invalid text representation", "22P02", "", store.ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Translate(&pgconn.PgError{Code: tc.code, ColumnName: tc.column})
			if !errors.Is(err, tc.want) {
				t.Errorf("Translate(%s) = %v, want errors.Is(_, %v)", tc.code, err, tc.want)
			}
		})
	}
}

// TestTranslateLeavesUnknownErrorsAlone keeps an unrecognized driver error
// classifiable as transient by callers.
func TestTranslateLeavesUnknownErrorsAlone(t *testing.T) {
	cause := errors.New("connection reset")
	if got := store.Translate(cause); !errors.Is(got, cause) {
		t.Errorf("Translate(unknown) = %v, want the original error", got)
	}
	if got := store.Translate(&pgconn.PgError{Code: "08006"}); errors.Is(got, store.ErrInvalid) ||
		errors.Is(got, store.ErrConflict) || errors.Is(got, store.ErrNotFound) {
		t.Errorf("Translate(08006) = %v, want it left unclassified", got)
	}
}

// TestTranslateIsIdempotent: an already-classified error passes through.
func TestTranslateIsIdempotent(t *testing.T) {
	for _, sentinel := range []error{store.ErrNotFound, store.ErrConflict, store.ErrInvalid} {
		if got := store.Translate(sentinel); !errors.Is(got, sentinel) {
			t.Errorf("Translate(%v) = %v", sentinel, got)
		}
	}
}
