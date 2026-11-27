package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
)

func (s *server) ingest(w http.ResponseWriter, r *http.Request) {
	if s.d.Submitter == nil {
		fail(w, r, unavailable())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.c.UploadTimeout)
	defer cancel()
	if s.c.UploadLimit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.c.UploadLimit)
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		fail(w, r, &apiError{status: 429, code: "rate_limited", message: "Upload limit reached"})
		return
	}
	media, params, err := mimeParse(r)
	if err != nil || !strings.HasPrefix(media, "multipart/form-data") {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Expected multipart PDF upload"})
		return
	}
	mr := multipartReader(r, params)
	if mr == nil {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Malformed multipart request"})
		return
	}
	var file []byte
	found := false
	for {
		part, e := mr.NextPart()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			fail(w, r, bodyReadError(e))
			return
		}
		name := part.FormName()
		if name != "file" || found || part.FileName() == "" {
			part.Close()
			fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Expected exactly one file part named file"})
			return
		}
		found = true
		data, e := io.ReadAll(io.LimitReader(part, s.c.SourceLimit+1))
		_ = part.Close()
		if e != nil {
			fail(w, r, bodyReadError(e))
			return
		}
		if int64(len(data)) > s.c.SourceLimit {
			fail(w, r, &apiError{status: 413, code: "request_too_large", message: "PDF file too large"})
			return
		}
		file = data
	}
	if !found || len(file) < 5 || string(file[:5]) != "%PDF-" {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Upload must contain a PDF file"})
		return
	}
	if e := ctx.Err(); e != nil {
		fail(w, r, unavailable())
		return
	}
	sub, err := s.d.Submitter.Submit(ctx, strings.NewReader(string(file)), int64(len(file)))
	if err != nil {
		fail(w, r, classifySubmit(err))
		return
	}
	now := time.Now().UTC()
	location := "/api/v1/jobs/" + sub.JobID.String()
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": sub.JobID, "state": "queued", "created_at": now, "updated_at": now})
}
func (s *server) jobs(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	limit, e := parseLimit(r, 50, 100)
	if e != nil {
		fail(w, r, e)
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && !validState(state) {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Invalid state filter"})
		return
	}
	page, err := s.d.Domain.ListJobs(r.Context(), limit, r.URL.Query().Get("cursor"), state)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	writeJSON(w, 200, page)
}
func (s *server) job(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	id, e := parseUUID(r.PathValue("job_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	v, err := s.d.Domain.GetJob(r.Context(), id)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	writeJSON(w, 200, jobDTO(v))
}
func (s *server) exams(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	limit, e := parseLimit(r, 50, 100)
	if e != nil {
		fail(w, r, e)
		return
	}
	page, err := s.d.Domain.ListExams(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	writeJSON(w, 200, page)
}
func (s *server) exam(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	id, e := parseUUID(r.PathValue("exam_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	v, err := s.d.Domain.GetExam(r.Context(), id)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	writeJSON(w, 200, v)
}
func (s *server) source(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil || s.d.Objects == nil {
		fail(w, r, unavailable())
		return
	}
	id, e := parseUUID(r.PathValue("exam_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	obj, err := s.d.Domain.GetSource(r.Context(), id)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	if storage.ValidateKey(obj.Key) != nil || !storage.ValidSHA256(obj.SHA256) {
		fail(w, r, internal(r, errors.New("invalid source registry metadata")))
		return
	}
	rc, err := s.d.Objects.Get(r.Context(), obj.Key, s.c.SourceLimit)
	if err != nil {
		fail(w, r, mapObject(err))
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, s.c.SourceLimit+1))
	if err != nil {
		fail(w, r, mapObject(err))
		return
	}
	if int64(len(data)) > s.c.SourceLimit {
		fail(w, r, &apiError{status: 413, code: "request_too_large", message: "Stored source exceeds service limit"})
		return
	}
	if !strings.HasPrefix(string(data), "%PDF-") {
		fail(w, r, internal(r, errors.New("source object is not pdf")))
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != obj.SHA256 {
		fail(w, r, internal(r, errors.New("source digest mismatch")))
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+obj.SHA256+`"`)
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
func (s *server) patchItem(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	exam, e := parseUUID(r.PathValue("exam_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	item, e := parseUUID(r.PathValue("item_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	revision, e := parseETag(r.Header.Get("If-Match"), "item", item.String())
	if e != nil {
		fail(w, r, e)
		return
	}
	var req patchRequest
	if e = strictJSON(r, s.c.JSONLimit, &req); e != nil {
		fail(w, r, e)
		return
	}
	patch, e := req.domainPatch()
	if e != nil {
		fail(w, r, e)
		return
	}
	res, err := s.d.Domain.PatchItem(r.Context(), exam, item, revision, patch)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	etag := itemETag(res.ItemID, res.Revision)
	w.Header().Set("ETag", etag)
	writeJSON(w, 200, map[string]any{"item_id": res.ItemID, "question_id": res.QuestionID, "revision": res.Revision, "etag": etag, "item": res.Content})
}
func (s *server) asset(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil || s.d.Objects == nil {
		fail(w, r, unavailable())
		return
	}
	id := r.PathValue("asset_id")
	if storage.ValidateAssetID(id) != nil {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Invalid asset identifier"})
		return
	}
	variant := storage.Variant(r.URL.Query().Get("variant"))
	if variant == "" {
		variant = storage.VariantActive
	}
	if variant != storage.VariantActive && variant != storage.VariantRaw && variant != storage.VariantClean {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Invalid asset variant"})
		return
	}
	rec, err := s.d.Domain.GetAsset(r.Context(), id)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	key, digest, err := rec.Select(variant)
	if err != nil {
		if errors.Is(err, storage.ErrVariantUnavailable) {
			fail(w, r, &apiError{status: 404, code: "variant_not_found", message: "Asset variant not found"})
		} else {
			fail(w, r, internal(r, err))
		}
		return
	}
	if storage.ValidateKey(key) != nil || !storage.ValidSHA256(digest) || !assetTypes[strings.ToLower(rec.MediaType)] {
		fail(w, r, internal(r, errors.New("invalid asset registry metadata")))
		return
	}
	rc, err := s.d.Objects.Get(r.Context(), key, s.c.AssetLimit)
	if err != nil {
		fail(w, r, mapObject(err))
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, s.c.AssetLimit+1))
	if err != nil || int64(len(data)) > s.c.AssetLimit {
		fail(w, r, mapObject(err))
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		fail(w, r, internal(r, errors.New("asset digest mismatch")))
		return
	}
	etag := `"` + digest + `"`
	w.Header().Set("Content-Type", strings.ToLower(rec.MediaType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("ETag", etag)
	if variant == storage.VariantActive {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
func (s *server) activeVariant(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil {
		fail(w, r, unavailable())
		return
	}
	id := r.PathValue("asset_id")
	if storage.ValidateAssetID(id) != nil {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Invalid asset identifier"})
		return
	}
	revision, e := parseETag(r.Header.Get("If-Match"), "asset", id)
	if e != nil {
		fail(w, r, e)
		return
	}
	var req struct {
		Variant storage.Variant `json:"variant"`
	}
	if e := strictJSON(r, s.c.JSONLimit, &req); e != nil {
		fail(w, r, e)
		return
	}
	if req.Variant != storage.VariantRaw && req.Variant != storage.VariantClean {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Invalid asset variant"})
		return
	}
	rec, err := s.d.Domain.SetActiveVariant(r.Context(), id, req.Variant, revision)
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	dto := assetDTO(rec)
	w.Header().Set("ETag", dto.ETag)
	writeJSON(w, 200, dto)
}
func (s *server) render(w http.ResponseWriter, r *http.Request) {
	if s.d.Domain == nil || s.d.Renderer == nil || s.d.Validator == nil {
		fail(w, r, unavailable())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.c.RenderTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	examID, e := parseUUID(r.PathValue("exam_id"))
	if e != nil {
		fail(w, r, e)
		return
	}
	var raw json.RawMessage
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Content-Type must be application/json"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.c.JSONLimit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, r, bodyReadError(err))
		return
	}
	if err = s.d.Validator.ValidateLayout(data); err != nil {
		fail(w, r, &apiError{status: 422, code: "validation_failed", message: "Request validation failed", details: []errorDetail{{Path: "layout", Code: "invalid_layout", Message: "layout specification is invalid"}}})
		return
	}
	if err = json.Unmarshal(data, &raw); err != nil {
		fail(w, r, &apiError{status: 400, code: "malformed_request", message: "Malformed JSON request"})
		return
	}
	layout, err := s.d.Validator.DecodeLayoutSpec(strings.NewReader(string(data)))
	if err != nil {
		fail(w, r, &apiError{status: 422, code: "validation_failed", message: "Request validation failed"})
		return
	}
	var doc ir.ExamDocumentIR
	err = s.d.Domain.WithExam(r.Context(), examID, func(_ context.Context, exam ir.ExamDocumentIR) error { doc = exam; return nil })
	if err != nil {
		fail(w, r, mapStore(err))
		return
	}
	select {
	case s.renderSlots <- struct{}{}:
		defer func() { <-s.renderSlots }()
	default:
		fail(w, r, &apiError{status: 429, code: "rate_limited", message: "Render limit reached"})
		return
	}
	artifact, err := s.d.Renderer.Render(r.Context(), doc, layout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, render.ErrRenderTimeout) || errors.Is(err, render.ErrQueueTimeout) {
			fail(w, r, unavailable())
		} else {
			fail(w, r, internal(r, err))
		}
		return
	}
	if artifact == nil || artifact.MIMEType != render.MIMEPDF || len(artifact.Bytes) < 5 || string(artifact.Bytes[:5]) != "%PDF-" {
		fail(w, r, internal(r, errors.New("renderer returned invalid pdf")))
		return
	}
	sum := sha256.Sum256(artifact.Bytes)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	warningsHeader(w, artifact.Warnings)
	w.WriteHeader(200)
	_, _ = w.Write(artifact.Bytes)
}

func mimeParse(r *http.Request) (string, map[string]string, error) {
	return mime.ParseMediaType(r.Header.Get("Content-Type"))
}
func multipartReader(r *http.Request, params map[string]string) *multipart.Reader {
	boundary := params["boundary"]
	if boundary == "" {
		return nil
	}
	return multipart.NewReader(r.Body, boundary)
}
func bodyReadError(err error) *apiError {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return &apiError{status: 413, code: "request_too_large", message: "Request body too large"}
	}
	return &apiError{status: 400, code: "malformed_request", message: "Malformed request body"}
}
func parseLimit(r *http.Request, def, max int) (int, *apiError) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 || n > max {
		return 0, &apiError{status: 400, code: "malformed_request", message: "Invalid limit"}
	}
	return n, nil
}
func validState(v string) bool {
	switch v {
	case store.JobQueued, store.JobParsing, store.JobValidating, store.JobPersisted, store.JobFailed:
		return true
	}
	return false
}
func classifySubmit(err error) *apiError {
	if errors.Is(err, storage.ErrInvalid) {
		return &apiError{status: 422, code: "validation_failed", message: "PDF could not be accepted"}
	}
	return unavailable()
}
func mapObject(err error) *apiError {
	if errors.Is(err, storage.ErrNotFound) {
		return &apiError{status: 404, code: "not_found", message: "Resource not found"}
	}
	if errors.Is(err, storage.ErrTooLarge) {
		return &apiError{status: 413, code: "request_too_large", message: "Stored object exceeds service limit"}
	}
	return unavailable()
}
func jobDTO(j store.JobSummary) JobDTO {
	var summary, code *string
	if j.ErrorMessage != "" {
		v := storage.Redact(j.ErrorMessage, 256)
		summary = &v
	}
	if j.ErrorCode != "" {
		v := j.ErrorCode
		if len(v) > 64 {
			v = "processing_failed"
		}
		code = &v
	}
	return JobDTO{JobID: j.JobID, State: j.State, AttemptCount: j.AttemptCount, ExamID: j.ExamID, ErrorCode: code, ErrorSummary: summary, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
}
func itemETag(id uuid.UUID, rev int64) string { return fmt.Sprintf(`"item-%s-r%d"`, id, rev) }
func assetETag(id string, rev int64) string   { return fmt.Sprintf(`"asset-%s-r%d"`, id, rev) }
func assetDTO(rec storage.AssetRecord) AssetMetadata {
	return AssetMetadata{AssetID: rec.AssetID, ActiveVariant: rec.ActiveVariant, AvailableVariants: rec.Available(), Revision: rec.Revision, ETag: assetETag(rec.AssetID, rec.Revision)}
}
