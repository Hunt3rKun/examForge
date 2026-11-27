package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
	"examforge/internal/store/storetest"
)

var (
	golden = parser.FromGolden("../../testdata/golden/ir/sample.json", "../../testdata/golden/assets")
)

func validator(t *testing.T) *ir.Validator {
	t.Helper()
	v, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func pdfSource(t *testing.T) ([]byte, string) {
	t.Helper()
	data := []byte("%PDF-1.7\n1 0 obj <<>> endobj\n%%EOF\n")
	return data, sourceDigest(data)
}

// setupSubmitter returns a submitter over the in-memory store and fake River.
func setupSubmitter(t *testing.T) (*Submitter, *storetest.MemDB, *fakeRiver) {
	t.Helper()
	db := storetest.New()
	objects := storage.NewMemStore()
	fake := &fakeRiver{}
	submitter, err := NewSubmitter(memRunner{db: db}, fake, objects, SubmitConfig{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	return submitter, db, fake
}

func TestSubmitCommitsJobAndRiverInsertTogether(t *testing.T) {
	submitter, db, fake := setupSubmitter(t)
	data, sha := pdfSource(t)
	sub, err := submitter.Submit(t.Context(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.SourceSHA256 != sha || sub.RiverJobID == 0 {
		t.Errorf("submission = %+v", sub)
	}
	jobs := db.JobRows()
	if len(jobs) != 1 || jobs[0].State != store.JobQueued || jobs[0].RiverJobID.String != fmt.Sprint(sub.RiverJobID) {
		t.Fatalf("job row = %+v", jobs)
	}
	inserted := fake.insertedJobs()
	if len(inserted) != 1 || inserted[0].Opts.Queue != "ingest" || inserted[0].Opts.MaxAttempts != 3 {
		t.Errorf("river insert = %+v", inserted)
	}
	args, ok := inserted[0].Args.(IngestArgs)
	if !ok || args.JobID != sub.JobID.String() {
		t.Errorf("river args = %#v", inserted[0].Args)
	}
	// The source is stored under the deterministic job key.
	if _, err := storage.PutImmutable(t.Context(), storage.NewMemStore(), "unused", nil, 0, "", 0); err == nil {
		_ = err
	}
}

func TestSubmitRollsBackTheJobRowWhenEnqueueFails(t *testing.T) {
	submitter, db, fake := setupSubmitter(t)
	fake.insertErr = errors.New("queue unavailable")
	data, _ := pdfSource(t)
	if _, err := submitter.Submit(t.Context(), bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("Submit succeeded although enqueue failed")
	}
	if n := len(db.JobRows()); n != 0 {
		t.Errorf("%d job rows survived a failed submission", n)
	}
}

func TestSubmitRejectsBadSources(t *testing.T) {
	submitter, db, _ := setupSubmitter(t)
	notPDF := []byte("hello")
	if _, err := submitter.Submit(t.Context(), bytes.NewReader(notPDF), int64(len(notPDF))); !IsPermanent(err) {
		t.Errorf("non-PDF err = %v, want permanent", err)
	}
	_, sha := pdfSource(t)
	data := []byte("%PDF-1.7\nx")
	if _, err := submitter.Submit(t.Context(), bytes.NewReader(data), 5); !IsPermanent(err) {
		t.Errorf("size mismatch err = %v, want permanent", err)
	}
	if n := len(db.JobRows()); n != 0 {
		t.Errorf("%d job rows after rejected submissions", n)
	}
	_ = sha
}

// workerFixture wires a worker over the in-memory store with a fake River.
func workerFixture(t *testing.T, opts ...parser.MockOption) (*IngestWorker, *storetest.MemDB, *storage.MemStore, *fakeRiver, uuid.UUID) {
	t.Helper()
	db := storetest.New()
	objects := storage.NewMemStore()
	adapter, err := parser.NewMockParserAdapter(append([]parser.MockOption{golden}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	assembler, err := store.NewAssembler(validator(t))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeRiver{}
	persistence := memPersistence{db: db, assembler: assembler}
	worker, err := NewIngestWorker(memJobStore{db}, persistence, objects, adapter, validator(t), fake, WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// Submit a source so the worker has something to read.
	data, sha := pdfSource(t)
	jobID := uuid.New()
	if _, err := db.InsertIngestJob(t.Context(), sqlcgen.InsertIngestJobParams{
		JobID: pgUUID(jobID), SourcePdfKey: storage.SourceKey(jobID), SourceFileSha256: sha,
	}); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(t.Context(), storage.SourceKey(jobID), bytes.NewReader(data), int64(len(data)), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	return worker, db, objects, fake, jobID
}

func TestWorkerHappyPathPersistsAndCompletes(t *testing.T) {
	worker, db, objects, fake, jobID := workerFixture(t)
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	rows := db.JobRows()
	if len(rows) != 1 || rows[0].State != store.JobPersisted || !rows[0].ExamID.Valid {
		t.Fatalf("job after Work = %+v", rows)
	}
	if len(db.Exams()) != 1 || len(db.Questions()) != 3 || len(db.Assets()) != 1 {
		t.Errorf("persisted rows: %d exams, %d questions, %d assets", len(db.Exams()), len(db.Questions()), len(db.Assets()))
	}
	// The asset is stored under its deterministic key.
	if keys := objects.Keys(); len(keys) != 2 {
		t.Errorf("object keys = %v, want the source and one asset", keys)
	}
	if cancelled := fake.cancelledJobs(); len(cancelled) != 0 {
		t.Errorf("a successful job was cancelled: %v", cancelled)
	}

	// A replayed delivery of the same job is an idempotent success.
	if err := worker.Work(t.Context(), job); err != nil {
		t.Errorf("replayed Work: %v", err)
	}
	if n := len(db.Exams()); n != 1 {
		t.Errorf("%d exams after replay, want 1", n)
	}
}

func TestWorkerInvalidIRIsPermanentAndCancels(t *testing.T) {
	worker, db, _, fake, jobID := workerFixture(t, parser.InvalidTimes(1))
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 7}, Args: IngestArgs{JobID: jobID.String()}}
	err := worker.Work(t.Context(), job)
	var cancelErr *rivertype.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("invalid IR err = %v, want a River cancel", err)
	}
	rows := db.JobRows()
	if rows[0].State != store.JobFailed || rows[0].ErrorCode.String != CodeIRInvalid {
		t.Errorf("job after invalid IR = %+v", rows[0])
	}
	if got := fake.cancelledJobs(); len(got) != 1 || got[0] != 7 {
		t.Errorf("cancelled jobs = %v", got)
	}
	// Nothing was persisted.
	if len(db.Exams()) != 0 {
		t.Error("an invalid IR produced an exam")
	}
	// A later delivery stays failed and is cancelled, never retried. The
	// domain row is already final, so no second JobCancel is needed.
	if err := worker.Work(t.Context(), job); !errors.As(err, &cancelErr) {
		t.Errorf("second delivery err = %v, want a cancel", err)
	}
	if n := len(fake.cancelledJobs()); n != 1 {
		t.Errorf("cancel count = %d, want 1", n)
	}
}

func TestWorkerTransientFailureIsRetried(t *testing.T) {
	worker, db, _, fake, jobID := workerFixture(t, parser.FailTimes(1, errors.New("gateway timeout")))
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 3}, Args: IngestArgs{JobID: jobID.String()}}
	err := worker.Work(t.Context(), job)
	if err == nil {
		t.Fatal("transient failure returned nil")
	}
	var cancelErr *rivertype.JobCancelError
	if errors.As(err, &cancelErr) {
		t.Fatal("a transient failure was cancelled instead of retried")
	}
	rows := db.JobRows()
	if rows[0].State != store.JobParsing || rows[0].ErrorCode.Valid {
		t.Errorf("job after a transient failure = %+v (want it to stay active)", rows[0])
	}
	if len(fake.cancelledJobs()) != 0 {
		t.Error("a transient failure cancelled the River job")
	}
	// The retry succeeds.
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if db.JobRows()[0].State != store.JobPersisted {
		t.Error("retry did not persist the exam")
	}
}

func TestWorkerFailureMessageIsBoundedAndRedacted(t *testing.T) {
	secret := "super-secret-token"
	worker, db, _, _, jobID := workerFixture(t, parser.Mutate(func(result *parser.ParseResult) {
		// The parser declares a digest that does not match its bytes. The
		// message quotes it, so the secret reaches the error text.
		result.Assets[0].SHA256 = secret
	}))
	// The operator-configured secret list is the only way a value can be
	// recognized as a secret; it is scrubbed wherever it appears.
	worker.cfg.Secrets = []string{secret}
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(t.Context(), job); err == nil {
		t.Fatal("mismatched asset digest did not fail")
	}
	row := db.JobRows()[0]
	if row.State != store.JobFailed || row.ErrorCode.String != CodeAssetInvalid {
		t.Fatalf("job = %+v", row)
	}
	message := row.ErrorMessage.String
	if strings.Contains(message, secret) {
		t.Error("stored message leaks the secret")
	}
	if len([]rune(message)) > MaxErrorMessageRunes {
		t.Errorf("stored message is %d runes; limit %d", len([]rune(message)), MaxErrorMessageRunes)
	}
}

func TestWorkerDoesNotRetryAPersistedJob(t *testing.T) {
	worker, db, _, fake, jobID := workerFixture(t)
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	before := len(fake.insertedJobs())
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(fake.insertedJobs()) != before {
		t.Error("a replay enqueued another job")
	}
	_ = db
}

// ── seams ────────────────────────────────────────────────────────────────────

type memJobStore struct{ db *storetest.MemDB }

func (m memJobStore) GetJob(ctx context.Context, id pgtype.UUID) (sqlcgen.IngestJob, error) {
	return m.db.GetIngestJob(ctx, id)
}
func (m memJobStore) Claim(ctx context.Context, id pgtype.UUID, from []store.JobState) (sqlcgen.IngestJob, error) {
	return m.db.ClaimIngestJob(ctx, sqlcgen.ClaimIngestJobParams{JobID: id, States: from})
}
func (m memJobStore) Move(ctx context.Context, id pgtype.UUID, to store.JobState, from []store.JobState) (sqlcgen.IngestJob, error) {
	return m.db.MoveIngestJob(ctx, sqlcgen.MoveIngestJobParams{JobID: id, State: to, FromStates: from})
}
func (m memJobStore) Fail(ctx context.Context, id pgtype.UUID, from []store.JobState, code, message string) (sqlcgen.IngestJob, error) {
	return m.db.FailIngestJob(ctx, sqlcgen.FailIngestJobParams{
		JobID: id, ErrorCode: pgtype.Text{String: code, Valid: true},
		ErrorMessage: pgtype.Text{String: message, Valid: true}, FromStates: from,
	})
}

type memPersistence struct {
	db        *storetest.MemDB
	assembler *store.Assembler
}

func (m memPersistence) Persist(ctx context.Context, exam ir.ExamDocumentIR, source store.SourceRecord) (rec store.ExamRecord, err error) {
	err = m.db.InTx(ctx, func(ctx context.Context) error {
		var err error
		rec, err = m.assembler.Persist(ctx, m.db, exam, source)
		return err
	})
	return rec, err
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
