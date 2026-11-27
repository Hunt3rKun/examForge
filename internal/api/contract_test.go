package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"examforge/internal/store"
)

func TestJobDTOUsesPublishedErrorSummaryFieldAndNullables(t *testing.T) {
	id := uuid.New()
	data, err := json.Marshal(jobDTO(store.JobSummary{JobID: id, State: store.JobFailed, ErrorCode: "parse_failed", ErrorMessage: "failed safely", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"error_code":"parse_failed"`, `"error_summary":"failed safely"`, `"exam_id":null`, `"started_at":null`, `"finished_at":null`} {
		if !strings.Contains(text, want) {
			t.Errorf("job JSON missing %s: %s", want, text)
		}
	}
	if strings.Contains(text, `"error_message"`) {
		t.Errorf("legacy error_message field serialized: %s", text)
	}
}

func TestRouteSpecificDeadlinesAreNotClampedByRequestDeadline(t *testing.T) {
	control, _ := NewServer(Dependencies{Config: Config{RequestTimeout: time.Millisecond}})
	s := control.impl
	for _, target := range []string{"POST /api/v1/ingest", "POST /api/v1/exams/00000000-0000-0000-0000-000000000001/render"} {
		method, path, _ := strings.Cut(target, " ")
		h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Deadline(); ok {
				t.Errorf("%s inherited the short global request deadline", target)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s status=%d", target, w.Code)
		}
	}
}

func TestMapRevisionConflictIncludesFreshETag(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		resource, want string
		revision       int64
	}{
		{"item " + id.String(), itemETag(id, 7), 7},
		{"asset ast_example", assetETag("ast_example", 4), 4},
	} {
		e := mapStore(&store.RevisionConflictError{Resource: tc.resource, Current: tc.revision})
		if e.status != 409 || e.headers["ETag"] == "" {
			t.Fatalf("missing conflict ETag: %+v", e)
		}
		if e.headers["ETag"] != tc.want {
			t.Fatalf("ETag=%q want %q", e.headers["ETag"], tc.want)
		}
	}
}
