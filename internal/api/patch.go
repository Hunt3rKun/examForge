package api

import (
	"bytes"
	"encoding/json"

	"examforge/internal/ir"
	"examforge/internal/store"
)

type patchRequest struct {
	Stem          json.RawMessage `json:"stem"`
	Options       json.RawMessage `json:"options"`
	Answer        json.RawMessage `json:"answer"`
	Analysis      json.RawMessage `json:"analysis"`
	Metadata      json.RawMessage `json:"metadata"`
	Provenance    json.RawMessage `json:"provenance"`
	DisplayNumber json.RawMessage `json:"display_number"`
	Score         json.RawMessage `json:"score"`
	AnswerSpaceMM json.RawMessage `json:"answer_space_mm"`
}

func (p patchRequest) domainPatch() (store.ItemPatch, *apiError) {
	var out store.ItemPatch
	bad := func() (store.ItemPatch, *apiError) {
		return store.ItemPatch{}, &apiError{status: 400, code: "malformed_request", message: "Malformed patch field"}
	}
	decode := func(raw json.RawMessage, dst any, nullable bool) (bool, *apiError) {
		if len(raw) == 0 {
			return false, nil
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if nullable {
				return true, nil
			}
			return false, &apiError{status: 400, code: "malformed_request", message: "Null is not allowed for this field"}
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			_, e := bad()
			return false, e
		}
		return true, nil
	}
	set := func(raw json.RawMessage, dst any, nullable bool) (bool, *apiError) { return decode(raw, dst, nullable) }
	if len(p.Stem) != 0 {
		var v []ir.ContentBlock
		if _, e := set(p.Stem, &v, false); e != nil {
			return out, e
		}
		out.Stem = &v
	}
	if len(p.Options) != 0 {
		var v []ir.Option
		if _, e := set(p.Options, &v, false); e != nil {
			return out, e
		}
		out.Options = &v
	}
	if len(p.Answer) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.Answer), []byte("null")) {
			out.ClearAnswer = true
		} else {
			var v ir.Answer
			if _, e := set(p.Answer, &v, false); e != nil {
				return out, e
			}
			out.Answer = &v
		}
	}
	if len(p.Analysis) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.Analysis), []byte("null")) {
			v := []ir.ContentBlock(nil)
			out.Analysis = &v
		} else {
			var v []ir.ContentBlock
			if _, e := set(p.Analysis, &v, false); e != nil {
				return out, e
			}
			out.Analysis = &v
		}
	}
	if len(p.Metadata) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.Metadata), []byte("null")) {
			var v map[string]any
			out.Metadata = &v
		} else {
			var v map[string]any
			if _, e := set(p.Metadata, &v, false); e != nil {
				return out, e
			}
			out.Metadata = &v
		}
	}
	if len(p.Provenance) != 0 {
		var prov map[string]json.RawMessage
		if _, e := set(p.Provenance, &prov, false); e != nil {
			return out, e
		}
		if len(prov) == 0 {
			return bad()
		}
		for key := range prov {
			if key != "review_status" {
				return bad()
			}
		}
		raw, ok := prov["review_status"]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return bad()
		}
		var v string
		if _, e := set(raw, &v, false); e != nil {
			return out, e
		}
		out.ReviewStatus = &v
	}
	if len(p.DisplayNumber) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.DisplayNumber), []byte("null")) {
			v := ""
			out.DisplayNumber = &v
		} else {
			var v string
			if _, e := set(p.DisplayNumber, &v, false); e != nil {
				return out, e
			}
			out.DisplayNumber = &v
		}
	}
	if len(p.Score) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.Score), []byte("null")) {
			v := float64(0)
			out.Score = &v
		} else {
			var v float64
			if _, e := set(p.Score, &v, false); e != nil {
				return out, e
			}
			out.Score = &v
		}
	}
	if len(p.AnswerSpaceMM) != 0 {
		if bytes.Equal(bytes.TrimSpace(p.AnswerSpaceMM), []byte("null")) {
			v := float64(0)
			out.AnswerSpaceMM = &v
		} else {
			var v float64
			if _, e := set(p.AnswerSpaceMM, &v, false); e != nil {
				return out, e
			}
			out.AnswerSpaceMM = &v
		}
	}
	if out.Score != nil && (*out.Score < 0 || *out.Score > 1e6) {
		return store.ItemPatch{}, &apiError{status: 422, code: "validation_failed", message: "Request validation failed"}
	}
	if out.Stem == nil && out.Options == nil && out.Answer == nil && !out.ClearAnswer && out.Analysis == nil && out.Metadata == nil && out.ReviewStatus == nil && out.DisplayNumber == nil && out.Score == nil && out.AnswerSpaceMM == nil {
		return store.ItemPatch{}, &apiError{status: 400, code: "malformed_request", message: "Patch must contain fields"}
	}
	return out, nil
}
