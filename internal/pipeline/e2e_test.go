package pipeline_test

import (
	"bytes"
	"context"
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
	"examforge/internal/parser"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
	"examforge/internal/store/storetest"
)

// TestOfflineIngestToPDF runs the whole slice-2 chain with no database and no
// network: source upload into an in-memory store, the ingest worker with the
// mock parser, the in-memory persistence transaction, assembly back into Exam
// IR, and a real Typst render through the S3-backed resolver.
func TestOfflineIngestToPDF(t *testing.T) {
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skipf("typst is not installed: %v", err)
	}
	ctx := t.Context()
	db := storetest.New()
	objects := storage.NewMemStore()

	validator, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	assembler, err := store.NewAssembler(validator)
	if err != nil {
		t.Fatal(err)
	}
	persister, err := storetest.NewPersister(db, assembler)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := parser.NewMockParserAdapter(parser.FromGolden(
		"../../testdata/golden/ir/sample.json", "../../testdata/golden/assets"))
	if err != nil {
		t.Fatal(err)
	}

	// Queue a job and store its source, exactly as the submitter does.
	source := []byte("%PDF-1.7\nmock source\n%%EOF\n")
	jobID := uuid.New()
	jobPG := pgtype.UUID{Bytes: jobID, Valid: true}
	if _, err := db.InsertIngestJob(ctx, sqlcgen.InsertIngestJobParams{
		JobID: jobPG, SourcePdfKey: storage.SourceKey(jobID), SourceFileSha256: storage.HashBytes(source),
	}); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, storage.SourceKey(jobID), bytes.NewReader(source), int64(len(source)), "application/pdf"); err != nil {
		t.Fatal(err)
	}

	worker, err := pipeline.NewIngestWorker(jobStore{db}, persister, objects, adapter, validator, nil, pipeline.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	job := &river.Job[pipeline.IngestArgs]{
		JobRow: &rivertype.JobRow{ID: 1}, Args: pipeline.IngestArgs{JobID: jobID.String()},
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("worker: %v", err)
	}
	rows := db.JobRows()
	if len(rows) != 1 || rows[0].State != store.JobPersisted || !rows[0].ExamID.Valid {
		t.Fatalf("job after ingest = %+v", rows)
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
	if err := validator.ValidateExamDTO(exam); err != nil {
		t.Fatalf("assembled exam is invalid: %v", err)
	}
	if len(exam.Assets) != 1 {
		t.Fatalf("assembled %d assets, want 1", len(exam.Assets))
	}

	// The resolver reads the asset the worker uploaded, by registry key only.
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
		t.Fatalf("renderer: %v", err)
	}
	artifact, err := renderer.Render(ctx, exam, layout)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(artifact.Bytes, []byte("%PDF-")) {
		t.Errorf("artifact does not start with %%PDF-: %q", artifact.Bytes[:min(8, len(artifact.Bytes))])
	}
	if !strings.Contains(artifact.MIMEType, "pdf") {
		t.Errorf("MIME type = %q", artifact.MIMEType)
	}
}

// jobStore adapts the in-memory store to the worker's job seam.
type jobStore struct{ db *storetest.MemDB }

func (j jobStore) GetJob(ctx context.Context, id pgtype.UUID) (sqlcgen.IngestJob, error) {
	return j.db.GetIngestJob(ctx, id)
}

func (j jobStore) Claim(ctx context.Context, id pgtype.UUID, from []store.JobState) (sqlcgen.IngestJob, error) {
	return j.db.ClaimIngestJob(ctx, sqlcgen.ClaimIngestJobParams{JobID: id, States: from})
}

func (j jobStore) Move(ctx context.Context, id pgtype.UUID, to store.JobState, from []store.JobState) (sqlcgen.IngestJob, error) {
	return j.db.MoveIngestJob(ctx, sqlcgen.MoveIngestJobParams{JobID: id, State: to, FromStates: from})
}

func (j jobStore) Fail(ctx context.Context, id pgtype.UUID, from []store.JobState, code, message string) (sqlcgen.IngestJob, error) {
	return j.db.FailIngestJob(ctx, sqlcgen.FailIngestJobParams{
		JobID: id, ErrorCode: pgtype.Text{String: code, Valid: true},
		ErrorMessage: pgtype.Text{String: message, Valid: true}, FromStates: from,
	})
}
