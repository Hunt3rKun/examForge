// Package api implements the bounded, security-conscious slice 4A HTTP API.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
)

const (
	defaultJSONLimit   = 1 << 20
	defaultUploadLimit = 50 << 20
	maxErrorDetails    = 32
)

// Dependencies are explicit offline-test seams. Domain is implemented by an
// adapter over Repository/Assembler transactions; ObjectStore remains the only
// object byte source and Submitter owns ingestion enqueue semantics.
type Dependencies struct {
	Submitter Submitter
	Domain    Domain
	Objects   storage.ObjectStore
	Renderer  render.Renderer
	Validator *ir.Validator
	Probe     Probe
	Logger    *slog.Logger
	Config    Config
}

type Submitter interface {
	Submit(context.Context, io.Reader, int64) (pipeline.Submission, error)
}

// Domain is the API-facing transaction boundary. Implementations must use
// Repository.Read/InTx and Assembler; returned values are transport-neutral and
// must not expose storage keys or raw database errors.
type Domain interface {
	ListJobs(context.Context, int, string, string) (JobPage, error)
	GetJob(context.Context, uuid.UUID) (store.JobSummary, error)
	ListExams(context.Context, int, string) (ExamPage, error)
	GetExam(context.Context, uuid.UUID) (ExamDetail, error)
	WithExam(context.Context, uuid.UUID, func(context.Context, ir.ExamDocumentIR) error) error
	GetSource(context.Context, uuid.UUID) (SourceObject, error)
	PatchItem(context.Context, uuid.UUID, uuid.UUID, int64, store.ItemPatch) (store.ItemEditResult, error)
	GetAsset(context.Context, string) (storage.AssetRecord, error)
	SetActiveVariant(context.Context, string, storage.Variant, int64) (storage.AssetRecord, error)
}

type Probe interface{ Ping(context.Context) error }
type Config struct {
	AllowedOrigins []string
	JSONLimit      int64
	UploadLimit    int64
	SourceLimit    int64
	AssetLimit     int64
	RequestTimeout time.Duration
	UploadTimeout  time.Duration
	RenderTimeout  time.Duration
	MaxConcurrent  int
	MaxUploads     int
	MaxRenders     int
	ReadyTimeout   time.Duration
}
type JobPage struct {
	Items      []JobDTO `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}
type JobDTO struct {
	JobID        uuid.UUID  `json:"job_id"`
	State        string     `json:"state"`
	AttemptCount int32      `json:"attempt_count"`
	ExamID       *uuid.UUID `json:"exam_id"`
	ErrorCode    *string    `json:"error_code"`
	ErrorSummary *string    `json:"error_summary"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}
type ExamPage struct {
	Items      []ExamListItem `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}
type ExamListItem struct {
	ExamID        uuid.UUID `json:"exam_id"`
	Title         string    `json:"title"`
	Subject       string    `json:"subject"`
	CreatedAt     time.Time `json:"created_at"`
	PendingReview int       `json:"pending_review_count"`
	SectionCount  int       `json:"section_count"`
	ItemCount     int       `json:"item_count"`
}
type ExamDetail struct {
	IR       ir.ExamDocumentIR `json:"ir"`
	Metadata ExamMetadata      `json:"metadata"`
	Items    []ItemMetadata    `json:"items"`
	Assets   []AssetMetadata   `json:"assets"`
}
type ExamMetadata struct {
	ExamID       uuid.UUID `json:"exam_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SourceSHA256 string    `json:"source_sha256"`
}
type ItemMetadata struct {
	SectionID  string    `json:"section_id"`
	Ordinal    int       `json:"ordinal"`
	ItemID     uuid.UUID `json:"item_id"`
	QuestionID string    `json:"question_id"`
	Revision   int64     `json:"revision"`
	ETag       string    `json:"etag"`
	Shared     bool      `json:"is_shared"`
}
type AssetMetadata struct {
	AssetID           string            `json:"asset_id"`
	ActiveVariant     storage.Variant   `json:"active_variant"`
	AvailableVariants []storage.Variant `json:"available_variants"`
	Revision          int64             `json:"revision"`
	ETag              string            `json:"etag"`
}
type SourceObject struct {
	Key    string
	SHA256 string
}

type errorDetail struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type errorEnvelope struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id"`
	Details   []errorDetail `json:"details,omitempty"`
}
type apiError struct {
	status        int
	code, message string
	details       []errorDetail
	headers       map[string]string
	cause         error
}

func (e *apiError) Error() string { return e.message }

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var assetTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/svg+xml": true}
var allowedMethods = "GET, POST, PATCH, OPTIONS"
var exposedHeaders = "ETag, Location, X-Request-Id, X-Render-Warning-Count, X-Render-Warnings"

// New constructs the route tree and middleware. Nil domain features are
// disabled safely and respond with a generic 503 rather than panicking.
func New(d Dependencies) http.Handler { _, handler := NewServer(d); return handler }

// Server controls readiness independently from HTTP serving lifecycle.
type Server struct{ impl *server }

func (s *Server) SetReady(ready bool) {
	if s != nil && s.impl != nil {
		s.impl.ready.Store(ready)
	}
}

// NewServer constructs the API and returns a readiness control handle.
func NewServer(d Dependencies) (*Server, http.Handler) {
	c := defaults(d.Config)
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	api := &server{d: d, c: c, ready: atomic.Bool{}, uploadSlots: make(chan struct{}, c.MaxUploads), renderSlots: make(chan struct{}, c.MaxRenders), concurrentSlots: make(chan struct{}, c.MaxConcurrent)}
	api.ready.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.readiness)
	mux.HandleFunc("POST /api/v1/ingest", api.ingest)
	mux.HandleFunc("GET /api/v1/jobs", api.jobs)
	mux.HandleFunc("GET /api/v1/jobs/{job_id}", api.job)
	mux.HandleFunc("GET /api/v1/exams", api.exams)
	mux.HandleFunc("GET /api/v1/exams/{exam_id}", api.exam)
	mux.HandleFunc("GET /api/v1/exams/{exam_id}/source-pdf", api.source)
	mux.HandleFunc("PATCH /api/v1/exams/{exam_id}/items/{item_id}", api.patchItem)
	mux.HandleFunc("POST /api/v1/exams/{exam_id}/render", api.render)
	mux.HandleFunc("GET /api/v1/assets/{asset_id}", api.asset)
	mux.HandleFunc("PATCH /api/v1/assets/{asset_id}/active-variant", api.activeVariant)
	return &Server{impl: api}, api.middleware(mux)
}

type server struct {
	d               Dependencies
	c               Config
	ready           atomic.Bool
	uploadSlots     chan struct{}
	renderSlots     chan struct{}
	concurrentSlots chan struct{}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *server) readiness(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() || s.d.Probe == nil {
		fail(w, r, &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.c.ReadyTimeout)
	defer cancel()
	if s.d.Probe.Ping(ctx) != nil {
		fail(w, r, &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"})
		return
	}
	if s.d.Objects == nil {
		fail(w, r, &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"})
		return
	}
	// A bounded, intentionally invalid-neutral probe is unavailable in ObjectStore;
	// Stat is used only with a server-owned key and ErrNotFound counts as reachable.
	_, err := s.d.Objects.Stat(ctx, "healthcheck")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		fail(w, r, &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}
func defaults(c Config) Config {
	if c.JSONLimit <= 0 {
		c.JSONLimit = defaultJSONLimit
	}
	if c.UploadLimit <= 0 {
		c.UploadLimit = defaultUploadLimit
	}
	if c.SourceLimit <= 0 {
		c.SourceLimit = storage.MaxSourceBytes
	}
	if c.AssetLimit <= 0 {
		c.AssetLimit = storage.MaxAssetBytes
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.UploadTimeout <= 0 {
		c.UploadTimeout = 2 * time.Minute
	}
	if c.RenderTimeout <= 0 {
		c.RenderTimeout = 45 * time.Second
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 64
	}
	if c.MaxUploads <= 0 {
		c.MaxUploads = 2
	}
	if c.MaxRenders <= 0 {
		c.MaxRenders = 2
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 2 * time.Second
	}
	if c.UploadLimit > storage.MaxSourceBytes+(1<<20) {
		c.UploadLimit = storage.MaxSourceBytes + (1 << 20)
	}
	return c
}

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			id = randomID()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if recover() != nil {
				if !sw.wrote {
					fail(sw, r, &apiError{status: 500, code: "internal_error", message: "Internal server error"})
				}
			}
			if !sw.wrote {
				sw.WriteHeader(200)
			}
			s.d.Logger.Info("http access", "method", r.Method, "route", routeTemplate(r), "status", sw.status, "duration_ms", time.Since(start).Milliseconds(), "request_id", id, "bytes_in", r.ContentLength, "bytes_out", sw.bytes, "error_code", sw.code)
		}()
		s.securityHeaders(sw)
		if s.c.MaxConcurrent > 0 {
			select {
			case s.concurrentSlots <- struct{}{}:
				defer func() { <-s.concurrentSlots }()
			default:
				fail(sw, r, &apiError{status: 429, code: "rate_limited", message: "Request limit reached"})
				return
			}
		}
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			sw.Header().Set("Access-Control-Allow-Origin", origin)
			sw.Header().Set("Vary", "Origin")
			sw.Header().Set("Access-Control-Expose-Headers", exposedHeaders)
		}
		if r.Method == http.MethodOptions {
			if origin == "" || !s.originAllowed(origin) {
				fail(sw, r, &apiError{status: 403, code: "malformed_request", message: "Origin is not allowed"})
				return
			}
			sw.Header().Set("Access-Control-Allow-Methods", allowedMethods)
			sw.Header().Set("Access-Control-Allow-Headers", "Content-Type, If-Match, If-None-Match, Range, X-Request-Id")
			sw.WriteHeader(204)
			return
		}
		// Upload and render have longer route-specific deadlines. Applying the
		// ordinary request deadline outside them would silently cap those routes.
		if (r.Method == http.MethodPost && r.URL.Path == "/api/v1/ingest") ||
			(r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/exams/") && strings.HasSuffix(r.URL.Path, "/render")) {
			next.ServeHTTP(sw, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.c.RequestTimeout)
		defer cancel()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

type requestIDKey struct{}
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
	code   string
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	n, e := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, e
}
func (s *server) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
}
func (s *server) originAllowed(origin string) bool {
	for _, v := range s.c.AllowedOrigins {
		if v != "*" && v == origin {
			return true
		}
	}
	return false
}
func randomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(b[:])
}
func routeTemplate(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}
func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, r *http.Request, e *apiError) {
	if e == nil {
		e = &apiError{status: 500, code: "internal_error", message: "Internal server error"}
	}
	if sw, ok := w.(*statusWriter); ok {
		sw.code = e.code
	}
	for key, value := range e.headers {
		w.Header().Set(key, value)
	}
	writeJSON(w, e.status, errorEnvelope{Code: e.code, Message: e.message, RequestID: requestID(r), Details: e.details})
}
func internal(r *http.Request, err error) *apiError {
	return &apiError{status: 500, code: "internal_error", message: "Internal server error", cause: err}
}
func unavailable() *apiError {
	return &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"}
}
func mapStore(err error) *apiError {
	var conflict *store.RevisionConflictError
	if errors.As(err, &conflict) {
		var etag string
		resource, id, ok := strings.Cut(conflict.Resource, " ")
		if ok && resource == "item" {
			if parsed, e := uuid.Parse(id); e == nil {
				etag = itemETag(parsed, conflict.Current)
			}
		}
		if ok && resource == "asset" {
			etag = assetETag(id, conflict.Current)
		}
		headers := map[string]string{}
		if etag != "" {
			headers["ETag"] = etag
		}
		return &apiError{status: 409, code: "revision_conflict", message: "Resource revision conflict", headers: headers}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &apiError{status: 404, code: "not_found", message: "Resource not found"}
	case errors.Is(err, store.ErrConflict):
		return &apiError{status: 409, code: "revision_conflict", message: "Resource revision conflict"}
	case errors.Is(err, store.ErrInvalid):
		return &apiError{status: 422, code: "validation_failed", message: "Request validation failed"}
	default:
		return &apiError{status: 503, code: "service_unavailable", message: "Service unavailable"}
	}
}
func parseUUID(raw string) (uuid.UUID, *apiError) {
	id, e := uuid.Parse(raw)
	if e != nil {
		return uuid.Nil, &apiError{status: 400, code: "malformed_request", message: "Invalid identifier"}
	}
	return id, nil
}
func parseETag(raw, kind, id string) (int64, *apiError) {
	if raw == "" {
		return 0, &apiError{status: 400, code: "malformed_request", message: "If-Match is required"}
	}
	prefix := `"` + kind + "-" + id + "-r"
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, `"`) {
		return 0, &apiError{status: 400, code: "malformed_request", message: "Invalid If-Match"}
	}
	n, e := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(raw, prefix), `"`), 10, 64)
	if e != nil || n <= 0 {
		return 0, &apiError{status: 400, code: "malformed_request", message: "Invalid If-Match"}
	}
	return n, nil
}
func strictJSON(r *http.Request, limit int64, dst any) *apiError {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return &apiError{status: 400, code: "malformed_request", message: "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if e := dec.Decode(dst); e != nil {
		var max *http.MaxBytesError
		if errors.As(e, &max) {
			return &apiError{status: 413, code: "request_too_large", message: "Request body too large"}
		}
		return &apiError{status: 400, code: "malformed_request", message: "Malformed JSON request"}
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return &apiError{status: 400, code: "malformed_request", message: "Malformed JSON request"}
	}
	return nil
}
func cleanWarning(value, fallback string) string {
	if value == "" || len(value) > 256 {
		return fallback
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return fallback
		}
	}
	return value
}
func warningsHeader(w http.ResponseWriter, warnings []render.RenderWarning) {
	w.Header().Set("X-Render-Warning-Count", strconv.Itoa(len(warnings)))
	items := make([]map[string]string, 0, len(warnings))
	for _, v := range warnings {
		items = append(items, map[string]string{"code": cleanWarning(v.Code, "warning"), "path": cleanWarning(v.Path, "unknown")})
	}
	b, e := json.Marshal(items)
	if e == nil && len(b) <= 4096 {
		w.Header().Set("X-Render-Warnings", string(b))
	}
}
