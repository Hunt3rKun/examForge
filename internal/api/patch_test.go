package api

import (
	"encoding/json"
	"testing"
)

func TestPatchNullClearsNullableFields(t *testing.T) {
	for _, tc := range []struct {
		body  string
		check func(*testing.T, patchRequest)
	}{
		{`{"answer":null}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || !got.ClearAnswer {
				t.Fatalf("answer null: patch=%+v err=%v", got, e)
			}
		}},
		{`{"analysis":null}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.Analysis == nil || *got.Analysis != nil {
				t.Fatalf("analysis null: patch=%+v err=%v", got, e)
			}
		}},
		{`{"metadata":null}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.Metadata == nil || *got.Metadata != nil {
				t.Fatalf("metadata null: patch=%+v err=%v", got, e)
			}
		}},
		{`{"display_number":null,"score":5}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.DisplayNumber == nil || *got.DisplayNumber != "" || got.Score == nil || *got.Score != 5 {
				t.Fatalf("nullable scalar null: patch=%+v err=%v", got, e)
			}
		}},
		{`{"answer_space_mm":null}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.AnswerSpaceMM == nil || *got.AnswerSpaceMM != 0 {
				t.Fatalf("answer-space null: patch=%+v err=%v", got, e)
			}
		}},
		{`{"provenance":{"review_status":"approved"}}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.ReviewStatus == nil || *got.ReviewStatus != "approved" {
				t.Fatalf("provenance patch: patch=%+v err=%v", got, e)
			}
		}},
		{`{"provenance":{"review_status":""}}`, func(t *testing.T, p patchRequest) {
			got, e := p.domainPatch()
			if e != nil || got.ReviewStatus == nil || *got.ReviewStatus != "" {
				t.Fatalf("review status clear: patch=%+v err=%v", got, e)
			}
		}},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var p patchRequest
			if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
				t.Fatal(err)
			}
			tc.check(t, p)
		})
	}
}

func TestPatchRejectsNullForRequiredFieldsAndTopLevelReviewStatus(t *testing.T) {
	for _, body := range []string{`{"stem":null}`, `{"options":null}`, `{"provenance":{}}`, `{"provenance":{"review_status":null}}`, `{"review_status":"approved"}`} {
		var p patchRequest
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		if _, e := p.domainPatch(); e == nil {
			t.Errorf("domainPatch(%s) unexpectedly succeeded", body)
		}
	}
}

func TestPatchRejectsScoreAboveRuntimeLimit(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"score":1000000}`, 0},
		{`{"score":1000000.01}`, 422},
	} {
		var p patchRequest
		if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
			t.Fatal(err)
		}
		_, e := p.domainPatch()
		if tc.want == 0 && e != nil {
			t.Fatalf("domainPatch(%s) error = %v", tc.body, e)
		}
		if tc.want != 0 && (e == nil || e.status != tc.want) {
			t.Fatalf("domainPatch(%s) error = %v, want status %d", tc.body, e, tc.want)
		}
	}
}
