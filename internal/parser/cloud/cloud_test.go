package cloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/vision/clean"
	"examforge/internal/vision/raster"
	"examforge/internal/vision/testpdf"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func testValidator(t *testing.T) *ir.Validator {
	t.Helper()
	v, err := ir.NewValidator("../../../schemas/exam-ir-v1.json", "../../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// dirtyPage builds a page image: white background, a gray watermark band, a
// red annotation stroke and a black figure block inside the figure bbox.
func dirtyPage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, white)
		}
	}
	gray := color.RGBA{R: 220, G: 220, B: 220, A: 255}
	for y := 0; y < h; y++ {
		for x := y; x < min(y+15, w); x++ {
			img.SetRGBA(x, y, gray)
		}
	}
	// Figure region: x in [0.5w, 0.8w], y in [0.2h, 0.6h].
	x1, x2 := w/2, w*4/5
	y1, y2 := h/5, h*3/5
	red := color.RGBA{R: 230, G: 25, B: 25, A: 255}
	for y := y1; y < y1+12 && y < y2; y++ {
		for x := x1; x < x2; x++ {
			img.SetRGBA(x, y, red)
		}
	}
	black := color.RGBA{A: 255}
	for y := y1 + 20; y < y2; y++ {
		for x := x1 + 10; x < x2-10; x++ {
			img.SetRGBA(x, y, black)
		}
	}
	return img
}

// fakeRasterizer serves pre-built page images without subprocesses.
type fakeRasterizer struct {
	pages []raster.Page
	err   error
}

func (f *fakeRasterizer) PageCount(context.Context, []byte) (int, error) {
	return len(f.pages), f.err
}

func (f *fakeRasterizer) Render(_ context.Context, _ []byte, _ raster.PageRange) ([]raster.Page, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pages, nil
}

func newFakeRasterizer(pageImages ...image.Image) *fakeRasterizer {
	f := &fakeRasterizer{}
	for i, img := range pageImages {
		f.pages = append(f.pages, raster.Page{Number: i + 1, DPI: 100, Image: img})
	}
	return f
}

// vlmServer is a scriptable OpenAI-compatible httptest endpoint. It records
// request metadata (never full bodies in assertions that could leak secrets)
// and replies per call kind: page extraction (has image), merge, repair.
type vlmServer struct {
	*httptest.Server
	mu            sync.Mutex
	pageReplies   map[int]string // page number -> raw model content
	mergeReplies  []string
	repairReplies []string
	pageCalls     int
	mergeCalls    int
	repairCalls   int
	status        int    // forced HTTP status; 0 = 200
	rawBody       string // forced raw response body
	maxLive       atomic.Int64
	live          atomic.Int64
	requests      []recordedRequest
	delay         time.Duration
	// repairHook, when set, answers repair requests with the request's own
	// candidate after applying the hook's mutation.
	repairHook func(map[string]any)
}

type recordedRequest struct {
	path        string
	auth        string
	hasImage    bool
	imageMIME   string
	imageBytes  int
	bodyBytes   int
	contentText string
}

func newVLMServer(t *testing.T) *vlmServer {
	t.Helper()
	s := &vlmServer{pageReplies: map[int]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live := s.live.Add(1)
		for {
			old := s.maxLive.Load()
			if live <= old || s.maxLive.CompareAndSwap(old, live) {
				break
			}
		}
		defer s.live.Add(-1)
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		rec := recordedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), bodyBytes: len(body)}
		var userText string
		for _, msg := range req.Messages {
			if msg.Role != "user" {
				continue
			}
			for _, part := range msg.Content {
				switch part.Type {
				case "image_url":
					rec.hasImage = true
					if part.ImageURL != nil {
						u := part.ImageURL.URL
						if strings.HasPrefix(u, "data:image/jpeg;base64,") {
							rec.imageMIME = "image/jpeg"
							raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u, "data:image/jpeg;base64,"))
							if err == nil {
								rec.imageBytes = len(raw)
							}
						} else {
							rec.imageMIME = "other"
						}
					}
				case "text":
					userText += part.Text
				}
			}
		}
		rec.contentText = userText
		s.mu.Lock()
		s.requests = append(s.requests, rec)
		status, rawBody := s.status, s.rawBody
		var content string
		switch {
		case rawBody != "":
			content = ""
		case rec.hasImage:
			var pageNum int
			fmt.Sscanf(userText, "Extract page %d.", &pageNum)
			content = s.pageReplies[pageNum]
			s.pageCalls++
		case strings.Contains(userText, `"candidate"`):
			if s.repairHook != nil {
				var rr repairRequest
				_ = json.Unmarshal([]byte(userText), &rr)
				var candidate map[string]any
				_ = json.Unmarshal(rr.Candidate, &candidate)
				s.repairHook(candidate)
				data, _ := json.Marshal(candidate)
				content = string(data)
			} else if len(s.repairReplies) > 0 {
				content = s.repairReplies[0]
				s.repairReplies = s.repairReplies[1:]
			}
			s.repairCalls++
		default:
			if len(s.mergeReplies) > 0 {
				content = s.mergeReplies[0]
				s.mergeReplies = s.mergeReplies[1:]
			}
			s.mergeCalls++
		}
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"forced"}`))
			return
		}
		if rawBody != "" {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 20},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *vlmServer) snapshot() ([]recordedRequest, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...), s.pageCalls, s.mergeCalls, s.repairCalls
}

func newTestParser(t *testing.T, s *vlmServer, rast raster.Rasterizer, cleaner clean.ImageCleaner, tune func(*Config)) *CloudAPIParser {
	t.Helper()
	cfg := Config{
		BaseURL: s.URL, APIKey: "test-key-not-a-secret", Model: "test-model",
		Rasterizer: rast, Cleaner: cleaner, Validator: testValidator(t),
		PageRatePerSecond: 1000,
	}
	if cleaner == nil {
		c, err := clean.NewHSVCleaner(clean.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Cleaner = c
	}
	if tune != nil {
		tune(&cfg)
	}
	p, err := NewCloudAPIParser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pdfSource(t *testing.T, pdf []byte) parser.Source {
	t.Helper()
	return parser.Source{
		StorageKey: "uploads/test/source.pdf",
		SHA256:     storage.HashBytes(pdf),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(pdf)), nil
		},
	}
}

// onePageExtraction is a valid single-page reply with one figure.
func onePageExtraction(page int) string {
	return fmt.Sprintf(`{"page":%d,"sections":[{"title":"一、选择题","questions":[
		{"kind":"single","display_number":"1","stem":[
			{"type":"text","markdown":"如图所示，求 $x$。"},
			{"type":"image_ref","asset_key":"fig_1","layout_hint":"center"}],
		 "options":[{"label":"A","content":[{"type":"text","markdown":"1"}]},{"label":"B","content":[{"type":"text","markdown":"2"}]}],
		 "answer":{"short_values":["B"]}}]}],
	 "assets":[{"key":"fig_1","media_type":"image/png","bbox":{"page":%d,"rect":[0.5,0.2,0.8,0.6]}}]}`, page, page)
}

func mustParse(t *testing.T, p *CloudAPIParser, pdf []byte) parser.ParseResult {
	t.Helper()
	res, err := p.Parse(t.Context(), pdfSource(t, pdf))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

var fakePDF = []byte("%PDF-1.4\nfake source for cloud parser tests\n%%EOF\n")

// ── happy path ───────────────────────────────────────────────────────────────

func TestParseHappyPathCleanedDualPayload(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = onePageExtraction(1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	var diag Diagnostics
	p.cfg.Diagnostics = func(d Diagnostics) { diag = d }
	res := mustParse(t, p, fakePDF)

	if err := testValidator(t).ValidateExamDTO(res.Exam); err != nil {
		t.Fatalf("result IR invalid: %v", err)
	}
	if len(res.Exam.Assets) != 1 || len(res.Assets) != 1 {
		t.Fatalf("assets: IR %d blobs %d", len(res.Exam.Assets), len(res.Assets))
	}
	for id, asset := range res.Exam.Assets {
		if !strings.HasPrefix(id, "ast_") {
			t.Errorf("asset ID %q is not server-generated", id)
		}
		if asset.Status != "cleaned" {
			t.Errorf("status = %q, want cleaned (fixture has removable marks)", asset.Status)
		}
		if asset.URI != "examforge-asset://"+id || asset.RawURI != "examforge-asset://"+id+"/raw" {
			t.Errorf("URIs are not resolver-neutral: %q %q", asset.URI, asset.RawURI)
		}
		if asset.AspectRatio <= 0 || asset.SuggestedWidthMM <= 0 {
			t.Errorf("geometry not backfilled: %+v", asset)
		}
		blob := res.Assets[0]
		if len(blob.RawData) == 0 || len(blob.CleanData) == 0 {
			t.Fatal("cleaned asset needs both payloads")
		}
		if bytes.Equal(blob.RawData, blob.CleanData) {
			t.Fatal("clean bytes equal raw bytes")
		}
		if blob.RawSHA256 != storage.HashBytes(blob.RawData) || blob.SHA256 != storage.HashBytes(blob.CleanData) {
			t.Fatal("digests do not match payloads")
		}
		// The raw crop keeps the red annotation; the clean variant removes it.
		assertRedPresence(t, blob.RawData, true)
		assertRedPresence(t, blob.CleanData, false)
	}
	reqs, pages, merges, repairs := s.snapshot()
	if pages != 1 || merges != 0 || repairs != 0 {
		t.Errorf("calls: pages=%d merges=%d repairs=%d; deterministic path must not call merge/repair", pages, merges, repairs)
	}
	for _, req := range reqs {
		if req.path != "/chat/completions" {
			t.Errorf("path = %q", req.path)
		}
		if req.auth != "Bearer test-key-not-a-secret" {
			t.Errorf("auth header missing/wrong")
		}
		if req.hasImage {
			if req.imageMIME != "image/jpeg" {
				t.Errorf("transfer image MIME = %q, want bounded JPEG", req.imageMIME)
			}
		}
	}
	if diag.Pages != 1 || diag.PromptTokens == 0 || diag.CompletionTokens == 0 {
		t.Errorf("diagnostics incomplete: %+v", diag)
	}
}

func assertRedPresence(t *testing.T, pngBytes []byte, want bool) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	red := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r > 0xa000 && g < 0x6000 && bl < 0x6000 {
				red++
			}
		}
	}
	if want && red == 0 {
		t.Error("raw crop lost the red annotation")
	}
	if !want && red > 0 {
		t.Errorf("clean variant still has %d red pixels", red)
	}
}

func TestCleanerFailureFallsBackToRawCropped(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = onePageExtraction(1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), errCleanerAdapter{}, nil)
	res := mustParse(t, p, fakePDF)
	for id, asset := range res.Exam.Assets {
		if asset.Status != "raw_cropped" {
			t.Errorf("asset %s status = %q, want raw_cropped fallback", id, asset.Status)
		}
		if asset.RawURI != "" {
			t.Errorf("raw_cropped asset carries raw_uri")
		}
	}
	blob := res.Assets[0]
	if len(blob.CleanData) != 0 || blob.SHA256 != "" {
		t.Error("fallback produced clean payload")
	}
	if len(res.Exam.Warnings) == 0 {
		t.Error("fallback did not record a warning")
	}
}

// errCleanerAdapter always fails, exercising the raw_cropped fallback.
type errCleanerAdapter struct{}

func (errCleanerAdapter) Clean(context.Context, io.Reader) (*clean.CleanResult, error) {
	return nil, fmt.Errorf("cleaner exploded")
}

// ── concurrency, ordering, cancellation ─────────────────────────────────────

func TestOutOfOrderPagesMergeStably(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"一、选择题","questions":[
		{"kind":"single","display_number":"1","stem":[{"type":"text","markdown":"第一题"}]}]}],"assets":[]}`
	s.pageReplies[2] = `{"page":2,"sections":[{"title":"一、选择题","questions":[
		{"kind":"single","display_number":"2","stem":[{"type":"text","markdown":"第二题"}]}]}],"assets":[]}`
	// Delay page 1 so page 2 finishes first.
	s.delay = 30 * time.Millisecond
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(200, 200), dirtyPage(200, 200)), nil, nil)
	first := mustParse(t, p, fakePDF)
	s.delay = 0
	second := mustParse(t, p, fakePDF)
	q1 := first.Exam.Sections[0].Questions
	q2 := second.Exam.Sections[0].Questions
	if len(q1) != 2 || len(q2) != 2 ||
		q1[0].DisplayNumber != "1" || q1[1].DisplayNumber != "2" ||
		q2[0].DisplayNumber != "1" || q2[1].DisplayNumber != "2" {
		t.Fatalf("merge order depends on completion order: %v vs %v", q1, q2)
	}
}

func TestPageConcurrencyBounded(t *testing.T) {
	s := newVLMServer(t)
	pages := make([]image.Image, 6)
	for i := range pages {
		pages[i] = dirtyPage(100, 100)
		s.pageReplies[i+1] = fmt.Sprintf(`{"page":%d,"sections":[{"title":"试题","questions":[
			{"kind":"single","display_number":"%d","stem":[{"type":"text","markdown":"题"}]}]}],"assets":[]}`, i+1, i+1)
	}
	s.delay = 20 * time.Millisecond
	p := newTestParser(t, s, newFakeRasterizer(pages...), nil, func(c *Config) { c.PageConcurrency = 2 })
	mustParse(t, p, fakePDF)
	if got := s.maxLive.Load(); got > 2 {
		t.Fatalf("observed %d concurrent requests; SetLimit is 2", got)
	}
}

func TestCancelledContextStopsRequests(t *testing.T) {
	s := newVLMServer(t)
	s.delay = 200 * time.Millisecond
	pages := make([]image.Image, 4)
	for i := range pages {
		pages[i] = dirtyPage(100, 100)
		s.pageReplies[i+1] = onePageExtraction(i + 1)
	}
	p := newTestParser(t, s, newFakeRasterizer(pages...), nil, func(c *Config) { c.PageConcurrency = 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := p.Parse(ctx, pdfSource(t, fakePDF))
	if err == nil {
		t.Fatal("cancelled parse succeeded")
	}
	if parser.IsPermanent(err) {
		t.Fatalf("cancellation classified permanent: %v", err)
	}
	// After the parse returned, no further requests may start.
	_, before, _, _ := s.snapshot()
	time.Sleep(300 * time.Millisecond)
	_, after, _, _ := s.snapshot()
	if after != before {
		t.Fatalf("requests continued after cancellation: %d -> %d", before, after)
	}
}

// ── merge boundary ───────────────────────────────────────────────────────────

func TestDeterministicContinuationNoMergeCall(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1","continues":true,"stem":[{"type":"text","markdown":"题干前半"}]}]}],"assets":[]}`
	s.pageReplies[2] = `{"page":2,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"","stem":[{"type":"text","markdown":"题干后半"}]}]}],"assets":[]}`
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100), dirtyPage(100, 100)), nil, nil)
	res := mustParse(t, p, fakePDF)
	_, _, merges, _ := s.snapshot()
	if merges != 0 {
		t.Fatalf("deterministic continuation called the merge model %d times", merges)
	}
	qs := res.Exam.Sections[0].Questions
	if len(qs) != 1 || len(qs[0].Stem) != 2 {
		t.Fatalf("continuation not joined: %+v", qs)
	}
}

func TestDeterministicAnswerBinding(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1","stem":[{"type":"text","markdown":"题"}]}]}],"assets":[]}`
	s.pageReplies[2] = `{"page":2,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"","answer_for":"1","stem":[{"type":"text","markdown":""}],
		 "answer":{"short_values":["B"]},"analysis":[{"type":"text","markdown":"因为所以"}]}]}],"assets":[]}`
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100), dirtyPage(100, 100)), nil, nil)
	res := mustParse(t, p, fakePDF)
	_, _, merges, _ := s.snapshot()
	if merges != 0 {
		t.Fatalf("explicit answer number called merge %d times", merges)
	}
	qs := res.Exam.Sections[0].Questions
	if len(qs) != 1 || qs[0].Answer == nil || qs[0].Answer.ShortValues[0] != "B" || len(qs[0].Analysis) != 1 {
		t.Fatalf("answer not bound: %+v", qs)
	}
}

func TestAmbiguousMergeUsesTextOnlyCall(t *testing.T) {
	s := newVLMServer(t)
	// Continues head on page 1; page 2 tail HAS a display number, so the
	// deterministic rule cannot join it — the text merge must decide.
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1","continues":true,"stem":[{"type":"text","markdown":"题干前半"}]}]}],"assets":[]}`
	s.pageReplies[2] = `{"page":2,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1-2","stem":[{"type":"text","markdown":"题干后半"}]}]}],"assets":[]}`
	s.mergeReplies = []string{`{"joins":[{"head_ref":"p1.q0","tail_ref":"p2.q0"}]}`}
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100), dirtyPage(100, 100)), nil, nil)
	res := mustParse(t, p, fakePDF)
	reqs, _, merges, _ := s.snapshot()
	if merges != 1 {
		t.Fatalf("merge calls = %d, want 1", merges)
	}
	for _, req := range reqs {
		if !req.hasImage && strings.Contains(req.contentText, "base64") {
			t.Error("text merge request carries image content")
		}
	}
	var mergeReq *recordedRequest
	for i := range reqs {
		if !reqs[i].hasImage && strings.Contains(reqs[i].contentText, `"questions"`) {
			mergeReq = &reqs[i]
		}
	}
	if mergeReq == nil {
		t.Fatal("no text merge request recorded")
	}
	if mergeReq.hasImage {
		t.Error("merge request contains an image")
	}
	qs := res.Exam.Sections[0].Questions
	if len(qs) != 1 || len(qs[0].Stem) != 2 {
		t.Fatalf("merge join not applied: %+v", qs)
	}
}

func TestAmbiguousUnresolvedStaysUnbound(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1","continues":true,"stem":[{"type":"text","markdown":"题干"}]}]}],"assets":[]}`
	s.pageReplies[2] = `{"page":2,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"2","stem":[{"type":"text","markdown":"另一题"}]}]}],"assets":[]}`
	// The model declines to join: ambiguity is preserved, not forced.
	s.mergeReplies = []string{`{}`}
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100), dirtyPage(100, 100)), nil, nil)
	res := mustParse(t, p, fakePDF)
	if len(res.Exam.Sections[0].Questions) != 2 {
		t.Fatal("unresolved ambiguity was force-joined")
	}
}

// ── repair loop ──────────────────────────────────────────────────────────────

func TestRepairFixesBBoxWithAccuratePaths(t *testing.T) {
	s := newVLMServer(t)
	// x1 >= x2: invalid geometry.
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"rect":[0.5,0.2,0.8,0.6]`, `"rect":[0.8,0.2,0.5,0.6]`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	// The repair reply is the request's own candidate with the bbox fixed.
	fixed := installRepairEcho(s, func(candidate map[string]any) {
		assets := candidate["assets"].(map[string]any)
		for _, a := range assets {
			asset := a.(map[string]any)
			bbox := asset["source_bbox"].(map[string]any)
			bbox["rect"] = []any{0.5, 0.2, 0.8, 0.6}
		}
	})
	defer fixed()
	res := mustParse(t, p, fakePDF)
	reqs, _, _, repairs := s.snapshot()
	if repairs != 1 {
		t.Fatalf("repair calls = %d, want 1", repairs)
	}
	var repairReq *recordedRequest
	for i := range reqs {
		if strings.Contains(reqs[i].contentText, `"candidate"`) {
			repairReq = &reqs[i]
		}
	}
	if repairReq == nil {
		t.Fatal("no repair request")
	}
	if !strings.Contains(repairReq.contentText, "source_bbox") || !strings.Contains(repairReq.contentText, "x1") {
		t.Errorf("repair request lacks accurate error paths: %s", repairReq.contentText[:min(400, len(repairReq.contentText))])
	}
	if repairReq.hasImage {
		t.Error("repair request carries images")
	}
	for _, asset := range res.Exam.Assets {
		if asset.SourceBBox.Rect[0] >= asset.SourceBBox.Rect[2] {
			t.Error("bbox not repaired")
		}
	}
}

// installRepairEcho makes the server answer repair requests with the
// request's own candidate JSON after applying mutate. It returns a restore
// func.
func installRepairEcho(s *vlmServer, mutate func(map[string]any)) func() {
	s.mu.Lock()
	s.repairHook = mutate
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.repairHook = nil
		s.mu.Unlock()
	}
}

func TestRepairCannotChangeServerIDs(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"rect":[0.5,0.2,0.8,0.6]`, `"rect":[0.8,0.2,0.5,0.6]`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	restore := installRepairEcho(s, func(candidate map[string]any) {
		assets := candidate["assets"].(map[string]any)
		for id, a := range assets {
			asset := a.(map[string]any)
			asset["asset_id"] = "ast_evil000000000000000000001"
			delete(assets, id)
			assets["ast_evil000000000000000000001"] = asset
		}
	})
	defer restore()
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) {
		t.Fatalf("ID-changing repair accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "repair_protected") {
		t.Errorf("error code = %v, want repair_protected", err)
	}
}

func TestRepairCannotAddAssetWithoutBBox(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"rect":[0.5,0.2,0.8,0.6]`, `"rect":[0.8,0.2,0.5,0.6]`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	restore := installRepairEcho(s, func(candidate map[string]any) {
		assets := candidate["assets"].(map[string]any)
		assets["ast_new0000000000000000000001"] = map[string]any{
			"asset_id": "ast_new0000000000000000000001", "media_type": "image/png",
			"uri": "examforge-asset://ast_new0000000000000000000001", "status": "raw_cropped",
		}
	})
	defer restore()
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) || !strings.Contains(err.Error(), "repair_protected") {
		t.Fatalf("asset-adding repair accepted: %v", err)
	}
}

func TestRepairExhaustionIsPermanent(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"rect":[0.5,0.2,0.8,0.6]`, `"rect":[0.8,0.2,0.5,0.6]`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	// Echo the broken candidate unchanged: every round fails validation.
	restore := installRepairEcho(s, func(map[string]any) {})
	defer restore()
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) {
		t.Fatalf("exhausted repair not permanent: %v", err)
	}
	_, _, _, repairs := s.snapshot()
	if repairs != DefaultMaxRepairRounds {
		t.Fatalf("repair rounds = %d, want %d", repairs, DefaultMaxRepairRounds)
	}
}

func TestBBoxPageBeyondDocumentRejected(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"bbox":{"page":1,`, `"bbox":{"page":9,`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, func(c *Config) { c.MaxRepairRounds = 1 })
	restore := installRepairEcho(s, func(map[string]any) {})
	defer restore()
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) {
		t.Fatalf("out-of-document bbox page accepted: %v", err)
	}
}

// ── HTTP error classification ────────────────────────────────────────────────

func TestHTTPStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		{400, true}, {401, true}, {403, true},
		{429, false}, {500, false}, {503, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			s := newVLMServer(t)
			s.status = tc.status
			p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100)), nil, nil)
			_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
			if err == nil {
				t.Fatal("error status accepted")
			}
			if parser.IsPermanent(err) != tc.permanent {
				t.Fatalf("HTTP %d classified permanent=%v, want %v: %v", tc.status, parser.IsPermanent(err), tc.permanent, err)
			}
		})
	}
}

func TestBadJSONAndOversizeResponse(t *testing.T) {
	s := newVLMServer(t)
	s.rawBody = `this is not json at all`
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100)), nil, nil)
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) {
		t.Fatalf("bad JSON not permanent: %v", err)
	}
	s2 := newVLMServer(t)
	s2.rawBody = strings.Repeat("x", 2<<20)
	p2 := newTestParser(t, s2, newFakeRasterizer(dirtyPage(100, 100)), nil, func(c *Config) { c.MaxResponseBytes = 1024 })
	_, err = p2.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) || !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("oversize response not permanent too_large: %v", err)
	}
}

func TestNetworkFailureIsTransient(t *testing.T) {
	s := newVLMServer(t)
	url := s.URL
	s.Close() // nothing listens anymore
	cfg := Config{
		BaseURL: url, APIKey: "k", Model: "m",
		Rasterizer: newFakeRasterizer(dirtyPage(100, 100)), Validator: testValidator(t),
	}
	c, _ := clean.NewHSVCleaner(clean.DefaultConfig())
	cfg.Cleaner = c
	p, err := NewCloudAPIParser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || parser.IsPermanent(err) {
		t.Fatalf("connection failure not transient: %v", err)
	}
}

func TestErrorsAndLogsNeverCarrySecretsOrImages(t *testing.T) {
	s := newVLMServer(t)
	s.status = 401
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100)), nil, nil)
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "test-key-not-a-secret") {
		t.Error("error carries the API key")
	}
	if strings.Contains(msg, s.URL) {
		t.Error("error carries the endpoint URL")
	}
	if strings.Contains(msg, "base64") {
		t.Error("error carries image payload marker")
	}
}

// ── ID remap ─────────────────────────────────────────────────────────────────

func TestSameModelKeyRemapsToDifferentIDsPerParse(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = onePageExtraction(1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	a := mustParse(t, p, fakePDF)
	b := mustParse(t, p, fakePDF)
	idsA, idsB := map[string]bool{}, map[string]bool{}
	for id := range a.Exam.Assets {
		idsA[id] = true
	}
	for id := range b.Exam.Assets {
		if idsA[id] {
			t.Fatalf("asset ID %q reused across parses", id)
		}
		idsB[id] = true
	}
	// Nested refs consistent.
	for _, res := range []parser.ParseResult{a, b} {
		for si := range res.Exam.Sections {
			for qi := range res.Exam.Sections[si].Questions {
				for _, block := range res.Exam.Sections[si].Questions[qi].Stem {
					if block.Type == "image_ref" {
						if _, ok := res.Exam.Assets[block.AssetID]; !ok {
							t.Fatalf("dangling ref %q", block.AssetID)
						}
					}
				}
			}
		}
	}
}

func TestDanglingAndUnreferencedAssetsRejected(t *testing.T) {
	s := newVLMServer(t)
	// References fig_missing which is never declared.
	s.pageReplies[1] = strings.Replace(onePageExtraction(1), `"asset_key":"fig_1"`, `"asset_key":"fig_missing"`, 1)
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	_, err := p.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) || !strings.Contains(err.Error(), "dangling") {
		t.Fatalf("dangling reference not permanent: %v", err)
	}
	s2 := newVLMServer(t)
	// Declares fig_1 and fig_2 but only references fig_1.
	s2.pageReplies[1] = strings.Replace(onePageExtraction(1),
		`"assets":[{"key":"fig_1"`,
		`"assets":[{"key":"fig_2","bbox":{"page":1,"rect":[0.1,0.1,0.2,0.2]}},{"key":"fig_1"`, 1)
	p2 := newTestParser(t, s2, newFakeRasterizer(dirtyPage(400, 300)), nil, nil)
	_, err = p2.Parse(t.Context(), pdfSource(t, fakePDF))
	if err == nil || !parser.IsPermanent(err) || !strings.Contains(err.Error(), "unreferenced") {
		t.Fatalf("unreferenced asset not permanent: %v", err)
	}
}

// ── config / budget ──────────────────────────────────────────────────────────

func TestBudgetInequalityEnforced(t *testing.T) {
	base := Config{
		BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m",
		Rasterizer: newFakeRasterizer(), Validator: testValidator(t),
	}
	c, _ := clean.NewHSVCleaner(clean.DefaultConfig())
	base.Cleaner = c
	cfg := base
	cfg.TotalParseTimeout = 5 * time.Minute
	cfg.WorkerReserve = time.Minute
	cfg.JobTimeout = 5 * time.Minute // 5m + 1m >= 5m: impossible
	if _, err := NewCloudAPIParser(cfg); err == nil {
		t.Fatal("impossible budget combination accepted")
	}
	cfg.JobTimeout = 7 * time.Minute
	if _, err := NewCloudAPIParser(cfg); err != nil {
		t.Fatalf("valid budget rejected: %v", err)
	}
	cfg = base
	cfg.MaxRepairRounds = HardMaxRepairRounds + 1
	if _, err := NewCloudAPIParser(cfg); err == nil {
		t.Fatal("repair rounds above hard ceiling accepted")
	}
}

func TestBudgetExhaustionBeforeExternalCallIsTransient(t *testing.T) {
	s := newVLMServer(t)
	s.pageReplies[1] = onePageExtraction(1)
	// A parent deadline shorter than the worker reserve: the budget check
	// must refuse the external call before it starts.
	p := newTestParser(t, s, newFakeRasterizer(dirtyPage(100, 100)), nil, func(c *Config) {
		c.WorkerReserve = 5 * time.Second
		c.TotalParseTimeout = 10 * time.Second
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := p.Parse(ctx, pdfSource(t, fakePDF))
	if err == nil {
		t.Fatal("budget-exhausted parse succeeded")
	}
	if parser.IsPermanent(err) {
		t.Fatalf("budget exhaustion classified permanent: %v", err)
	}
	_, pages, _, _ := s.snapshot()
	if pages != 0 {
		t.Fatalf("%d page calls started despite exhausted budget", pages)
	}
}

func TestEnvConfigRequiresAllThree(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvModel, "")
	if _, _, _, err := EnvConfig(); err == nil {
		t.Fatal("empty environment accepted")
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:9")
	t.Setenv(EnvAPIKey, "k")
	if _, _, _, err := EnvConfig(); err == nil || !strings.Contains(err.Error(), EnvModel) {
		t.Fatalf("missing model not reported: %v", err)
	}
	t.Setenv(EnvModel, "m")
	if _, _, _, err := EnvConfig(); err != nil {
		t.Fatalf("complete environment rejected: %v", err)
	}
}

// ── real mutool end-to-end ───────────────────────────────────────────────────

func TestRealRasterEndToEnd(t *testing.T) {
	for _, tool := range []string{"mutool", "pdfinfo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	pdf := testpdf.Build(1, 400, 300, [][3]float64{{1, 1, 1}})
	rast, err := raster.NewMuPDFRasterizer(raster.Config{DPI: 100})
	if err != nil {
		t.Fatal(err)
	}
	s := newVLMServer(t)
	s.pageReplies[1] = `{"page":1,"sections":[{"title":"试题","questions":[
		{"kind":"single","display_number":"1","stem":[{"type":"text","markdown":"题"}]}]}],"assets":[]}`
	p := newTestParser(t, s, rast, nil, nil)
	src := parser.Source{
		SHA256: storage.HashBytes(pdf),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(pdf)), nil
		},
	}
	res, err := p.Parse(t.Context(), src)
	if err != nil {
		t.Fatalf("Parse with real rasterizer: %v", err)
	}
	if len(res.Exam.Sections) != 1 {
		t.Fatalf("sections = %d", len(res.Exam.Sections))
	}
}
