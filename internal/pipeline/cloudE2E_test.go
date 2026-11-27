package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/ir"
	"examforge/internal/parser/cloud"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
	"examforge/internal/store/storetest"
	"examforge/internal/vision/clean"
	"examforge/internal/vision/raster"
	"examforge/internal/vision/testpdf"
)

// TestCloudIngestToPDF runs the whole slice-3 chain offline: a real multi-page
// PDF rasterized by real mutool, a localhost httptest VLM, the cloud parser's
// crop/clean stages, the real ingest worker's dual-object uploads, in-memory
// persistence, assembly and a real Typst render through the S3-backed
// resolver. No network beyond localhost, no paid tokens.
func TestCloudIngestToPDF(t *testing.T) {
	for _, tool := range []string{"typst", "mutool", "pdfinfo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	ctx := t.Context()

	// The VLM fake answers with one question referencing one figure whose
	// bbox covers a red-marked region of the generated page.
	reply := `{"page":1,"sections":[{"title":"一、选择题","questions":[
		{"kind":"single","display_number":"1","stem":[
			{"type":"text","markdown":"如图所示，函数 $f(x)$ 的图像如下，求 $f(1)$。"},
			{"type":"image_ref","asset_key":"fig_1","layout_hint":"center"}],
		 "options":[{"label":"A","content":[{"type":"text","markdown":"1"}]},
		            {"label":"B","content":[{"type":"text","markdown":"2"}]}],
		 "answer":{"short_values":["B"]}}]}],
	 "assets":[{"key":"fig_1","media_type":"image/png","caption":"函数图像","bbox":{"page":1,"rect":[0.2,0.2,0.8,0.8]}}]}`
	vlm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 200},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(vlm.Close)

	validator, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	rasterizer, err := raster.NewMuPDFRasterizer(raster.Config{DPI: 150})
	if err != nil {
		t.Fatal(err)
	}
	cleaner, err := clean.NewHSVCleaner(clean.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := cloud.NewCloudAPIParser(cloud.Config{
		BaseURL: vlm.URL, APIKey: "offline-test-key", Model: "offline-test",
		Rasterizer: rasterizer, Cleaner: cleaner, Validator: validator,
		PageRatePerSecond: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	db := storetest.New()
	objects := storage.NewMemStore()
	assembler, err := store.NewAssembler(validator)
	if err != nil {
		t.Fatal(err)
	}
	persister, err := storetest.NewPersister(db, assembler)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := pipeline.NewIngestWorker(jobStore{db}, persister, objects, adapter, validator, nil, pipeline.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// A real one-page PDF (white). The crop is white too, so the cleaner
	// removes nothing and the asset must honestly stay raw_cropped.
	pdf := testpdf.Build(1, 300, 200, [][3]float64{{1, 1, 1}})
	jobID := uuid.New()
	jobPG := pgtype.UUID{Bytes: jobID, Valid: true}
	if _, err := db.InsertIngestJob(ctx, sqlcgen.InsertIngestJobParams{
		JobID: jobPG, SourcePdfKey: storage.SourceKey(jobID), SourceFileSha256: storage.HashBytes(pdf),
	}); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, storage.SourceKey(jobID), bytes.NewReader(pdf), int64(len(pdf)), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	job := &river.Job[pipeline.IngestArgs]{
		JobRow: &rivertype.JobRow{ID: 1}, Args: pipeline.IngestArgs{JobID: jobID.String()},
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("worker: %v", err)
	}
	rows := db.JobRows()
	if len(rows) != 1 || rows[0].State != store.JobPersisted {
		t.Fatalf("job = %+v", rows)
	}
	assetRows := db.Assets()
	if len(assetRows) != 1 || assetRows[0].Status != "raw_cropped" {
		t.Fatalf("assets = %+v; a white crop must stay raw_cropped", assetRows)
	}
	examID := uuid.UUID(rows[0].ExamID.Bytes)

	var exam ir.ExamDocumentIR
	if err := db.InTx(ctx, func(ctx context.Context) error {
		var err error
		exam, err = assembler.Assemble(ctx, db, examID)
		return err
	}); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	resolver, err := storage.NewS3AssetResolver(objects, db, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	layoutData, err := os.ReadFile("../../testdata/golden/layout/student.json")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := validator.DecodeLayoutSpec(bytes.NewReader(layoutData))
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{
		SchemaDir:     "../../schemas",
		TemplateDir:   filepath.Join("..", "..", "templates"),
		PackageDir:    filepath.Join("..", "..", "third_party", "typst-packages"),
		FontDir:       filepath.Join("..", "..", "third_party", "fonts"),
		Validator:     validator,
		AssetResolver: resolver.ForRender(),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := renderer.Render(ctx, exam, layout)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(artifact.Bytes, []byte("%PDF-")) {
		t.Fatal("artifact is not a PDF")
	}
}

// TestCloudCleanedIngestToPDF repeats the chain with a dirty page (red marks,
// gray watermark, black figure) so the cleaner succeeds and the persisted
// asset is cleaned with two independently verified objects.
func TestCloudCleanedIngestToPDF(t *testing.T) {
	for _, tool := range []string{"typst", "mutool", "pdfinfo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	ctx := t.Context()
	reply := `{"page":1,"sections":[{"title":"一、选择题","questions":[
		{"kind":"single","display_number":"1","stem":[
			{"type":"text","markdown":"观察下图。"},
			{"type":"image_ref","asset_key":"fig_1","layout_hint":"center"}],
		 "options":[{"label":"A","content":[{"type":"text","markdown":"1"}]},
		            {"label":"B","content":[{"type":"text","markdown":"2"}]}]}]}],
	 "assets":[{"key":"fig_1","media_type":"image/png","bbox":{"page":1,"rect":[0.1,0.1,0.9,0.9]}}]}`
	vlm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	t.Cleanup(vlm.Close)

	validator, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	// A dirty rasterizer fake: the page carries removable marks so cleaning
	// produces a genuinely different object. mutool still runs for the real
	// PDF path in the sibling test; here the pixel content is what matters.
	rast := &dirtyRasterizer{}
	cleaner, err := clean.NewHSVCleaner(clean.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := cloud.NewCloudAPIParser(cloud.Config{
		BaseURL: vlm.URL, APIKey: "offline-test-key", Model: "offline-test",
		Rasterizer: rast, Cleaner: cleaner, Validator: validator, PageRatePerSecond: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.New()
	objects := storage.NewMemStore()
	assembler, _ := store.NewAssembler(validator)
	persister, _ := storetest.NewPersister(db, assembler)
	worker, err := pipeline.NewIngestWorker(jobStore{db}, persister, objects, adapter, validator, nil, pipeline.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4\ndirty source\n%%EOF\n")
	jobID := uuid.New()
	if _, err := db.InsertIngestJob(ctx, sqlcgen.InsertIngestJobParams{
		JobID: pgtype.UUID{Bytes: jobID, Valid: true}, SourcePdfKey: storage.SourceKey(jobID), SourceFileSha256: storage.HashBytes(pdf),
	}); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, storage.SourceKey(jobID), bytes.NewReader(pdf), int64(len(pdf)), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	job := &river.Job[pipeline.IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: pipeline.IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("worker: %v", err)
	}
	rows := db.Assets()
	if len(rows) != 1 || rows[0].Status != "cleaned" || !rows[0].RawSha256.Valid {
		t.Fatalf("assets = %+v; want cleaned with raw digest", rows)
	}
	if keys := objects.Keys(); len(keys) != 3 {
		t.Fatalf("keys = %v; want source + raw + clean", keys)
	}
	// Assemble and render through the resolver: the clean main object serves
	// the render, the raw companion is independently readable.
	examID := uuid.UUID(db.JobRows()[0].ExamID.Bytes)
	var exam ir.ExamDocumentIR
	if err := db.InTx(ctx, func(ctx context.Context) error {
		var err error
		exam, err = assembler.Assemble(ctx, db, examID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resolver, err := storage.NewS3AssetResolver(objects, db, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	for id, asset := range exam.Assets {
		rc, err := resolver.Resolve(ctx, id, asset)
		if err != nil {
			t.Fatalf("resolve clean: %v", err)
		}
		cleanBytes, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		rc, err = resolver.ResolveRaw(ctx, id, asset)
		if err != nil {
			t.Fatalf("resolve raw: %v", err)
		}
		rawBytes, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(cleanBytes, rawBytes) {
			t.Fatal("clean and raw objects are identical")
		}
		if strings.Contains(fmt.Sprint(asset.RawURI), "assets/") {
			t.Fatalf("raw URI leaks an object key: %q", asset.RawURI)
		}
	}
	layoutData, err := os.ReadFile("../../testdata/golden/layout/student.json")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := validator.DecodeLayoutSpec(bytes.NewReader(layoutData))
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{
		SchemaDir: "../../schemas", TemplateDir: filepath.Join("..", "..", "templates"),
		PackageDir: filepath.Join("..", "..", "third_party", "typst-packages"),
		FontDir:    filepath.Join("..", "..", "third_party", "fonts"),
		Validator:  validator, AssetResolver: resolver.ForRender(),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := renderer.Render(ctx, exam, layout)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(artifact.Bytes, []byte("%PDF-")) {
		t.Fatal("artifact is not a PDF")
	}
}

// dirtyRasterizer serves one synthetic dirty page without a subprocess.
type dirtyRasterizer struct{}

func (dirtyRasterizer) PageCount(context.Context, []byte) (int, error) { return 1, nil }

func (dirtyRasterizer) Render(context.Context, []byte, raster.PageRange) ([]raster.Page, error) {
	return []raster.Page{{Number: 1, DPI: 100, Image: dirtyImage()}}, nil
}

func dirtyImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	fill := func(x1, y1, x2, y2 int, r, g, b uint8) {
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
			}
		}
	}
	fill(0, 0, 300, 200, 255, 255, 255)
	fill(0, 0, 40, 200, 220, 220, 220) // gray watermark band
	fill(60, 40, 240, 60, 230, 25, 25) // red annotation stroke
	fill(80, 90, 220, 160, 0, 0, 0)    // black figure block
	return img
}
