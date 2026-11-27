package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store"
	"github.com/google/uuid"
)

type testDomain struct{}

func (*testDomain) ListJobs(context.Context, int, string, string) (JobPage, error) {
	return JobPage{}, nil
}
func (*testDomain) GetJob(context.Context, uuid.UUID) (store.JobSummary, error) {
	return store.JobSummary{}, nil
}
func (*testDomain) ListExams(context.Context, int, string) (ExamPage, error) { return ExamPage{}, nil }
func (*testDomain) GetExam(context.Context, uuid.UUID) (ExamDetail, error)   { return ExamDetail{}, nil }
func (*testDomain) WithExam(context.Context, uuid.UUID, func(context.Context, ir.ExamDocumentIR) error) error {
	return nil
}
func (*testDomain) GetSource(context.Context, uuid.UUID) (SourceObject, error) {
	return SourceObject{}, nil
}
func (*testDomain) PatchItem(context.Context, uuid.UUID, uuid.UUID, int64, store.ItemPatch) (store.ItemEditResult, error) {
	return store.ItemEditResult{}, nil
}
func (*testDomain) GetAsset(context.Context, string) (storage.AssetRecord, error) {
	return storage.AssetRecord{}, nil
}
func (*testDomain) SetActiveVariant(context.Context, string, storage.Variant, int64) (storage.AssetRecord, error) {
	return storage.AssetRecord{}, nil
}

func TestHealthAndRequestIDHeaders(t *testing.T) {
	h := New(Dependencies{})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-Id", "caller_123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("X-Request-Id") != "caller_123" {
		t.Fatalf("status=%d request-id=%q", w.Code, w.Header().Get("X-Request-Id"))
	}
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s=%q, want %q", k, got, want)
		}
	}
}

func TestMalformedIDUsesGenericEnvelope(t *testing.T) {
	h := New(Dependencies{Domain: &testDomain{}})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/nope", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"code":"malformed_request"`, `"request_id":`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s: %s", want, w.Body.String())
		}
	}
}

func TestCORSAllowlistAndReadinessSetter(t *testing.T) {
	ready, h := NewServer(Dependencies{Config: Config{AllowedOrigins: []string{"https://client.invalid"}}})
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/jobs", nil)
	r.Header.Set("Origin", "https://client.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://client.invalid" || w.Header().Get("Access-Control-Expose-Headers") != exposedHeaders {
		t.Fatalf("status=%d headers=%v", w.Code, w.Header())
	}
	ready.SetReady(false)
	r = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestDisallowedCORSAndETagValidation(t *testing.T) {
	h := New(Dependencies{Config: Config{AllowedOrigins: []string{"https://client.invalid"}}})
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/jobs", nil)
	r.Header.Set("Origin", "https://evil.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
	if _, e := parseETag(`"item-x-r1"`, "item", "different"); e == nil {
		t.Fatal("expected invalid ETag")
	}
	if _, e := parseETag(`"item-00000000-0000-0000-0000-000000000001-r2"`, "item", "00000000-0000-0000-0000-000000000001"); e != nil {
		t.Fatal(e)
	}
}

func TestErrorMappingDoesNotLeakCause(t *testing.T) {
	secret := errors.New("postgres password=bad endpoint https://db.invalid")
	e := internal(httptest.NewRequest(http.MethodGet, "/", nil), secret)
	if e.Error() != "Internal server error" {
		t.Fatalf("leaked error: %q", e.Error())
	}
}
