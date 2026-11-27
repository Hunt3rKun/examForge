//go:build integration

// Integration tests for slice 2. They require a real PostgreSQL and an
// S3-protocol service (SeaweedFS). Every connection value comes from the
// environment; nothing falls back to a default endpoint. Run them through
// scripts/verify-integration.sh, never as part of the offline gate.
package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge"
	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/migrate"
)

// integrationEnv is the operator-provided test configuration.
type integrationEnv struct {
	databaseURL       string
	s3Endpoint        string
	s3Bucket          string
	s3Region          string
	s3AccessKeyID     string
	s3SecretAccessKey string
}

func loadIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()
	env := integrationEnv{
		databaseURL:       os.Getenv("EXAMFORGE_TEST_DATABASE_URL"),
		s3Endpoint:        os.Getenv("EXAMFORGE_TEST_S3_ENDPOINT"),
		s3Bucket:          os.Getenv("EXAMFORGE_TEST_S3_BUCKET"),
		s3Region:          os.Getenv("EXAMFORGE_TEST_S3_REGION"),
		s3AccessKeyID:     os.Getenv("EXAMFORGE_TEST_S3_ACCESS_KEY_ID"),
		s3SecretAccessKey: os.Getenv("EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY"),
	}
	// A slice, not a map: the first missing name must be reported
	// deterministically, and no value is ever printed.
	for _, required := range []struct{ name, value string }{
		{"EXAMFORGE_TEST_DATABASE_URL", env.databaseURL},
		{"EXAMFORGE_TEST_S3_ENDPOINT", env.s3Endpoint},
		{"EXAMFORGE_TEST_S3_BUCKET", env.s3Bucket},
		{"EXAMFORGE_TEST_S3_REGION", env.s3Region},
		{"EXAMFORGE_TEST_S3_ACCESS_KEY_ID", env.s3AccessKeyID},
		{"EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY", env.s3SecretAccessKey},
	} {
		if required.value == "" {
			t.Fatalf("%s is required for integration tests", required.name)
		}
	}
	if !strings.EqualFold(os.Getenv("EXAMFORGE_TEST_S3_FORCE_PATH_STYLE"), "true") {
		t.Fatal("EXAMFORGE_TEST_S3_FORCE_PATH_STYLE must be true")
	}
	// The script checks these too; running the tests directly must not accept
	// a malformed value and then fail deep inside a driver.
	if !strings.HasPrefix(env.databaseURL, "postgres://") && !strings.HasPrefix(env.databaseURL, "postgresql://") {
		t.Fatal("EXAMFORGE_TEST_DATABASE_URL must start with postgres:// or postgresql://")
	}
	if !strings.HasPrefix(env.s3Endpoint, "http://") && !strings.HasPrefix(env.s3Endpoint, "https://") {
		t.Fatal("EXAMFORGE_TEST_S3_ENDPOINT must start with http:// or https://")
	}
	if len(env.s3Bucket) < 3 || len(env.s3Bucket) > 63 {
		t.Fatal("EXAMFORGE_TEST_S3_BUCKET must be 3 to 63 characters")
	}
	return env
}

// isolatedSchema creates a throwaway schema, points every pooled connection at
// it through search_path, and drops it at cleanup. River resolves its own
// schema from current_schema(), so both schemas land in the same namespace.
func isolatedSchema(t *testing.T, env integrationEnv) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, env.databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// River derives its LISTEN channel as "<schema>.<topic>", and PostgreSQL
	// caps an identifier at 63 bytes. Keep the schema short enough for the
	// longest River topic to fit.
	schema := "eft_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(env.databaseURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect isolated: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestIntegrationPreflight is the cheap connectivity probe run by
// `verify-integration.sh --check-only`. It proves the contract end to end
// without the full suite: the database accepts the URL, the role may create a
// throwaway schema, both migration sets apply, and the bucket is listable and
// writable with the given credentials.
func TestIntegrationPreflight(t *testing.T) {
	env := loadIntegrationEnv(t)
	ctx := t.Context()

	pool := isolatedSchema(t, env)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("postgres ping: %v", err)
	}
	migrations, err := migrate.Load(examforge.Migrations)
	if err != nil {
		t.Fatalf("load app migrations: %v", err)
	}
	applied, err := migrate.Up(ctx, pool, migrations)
	if err != nil {
		t.Fatalf("app migrations: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("applied %d of %d app migrations", len(applied), len(migrations))
	}
	if _, err := pipeline.RiverMigrate(ctx, pool, 0); err != nil {
		t.Fatalf("river migrations: %v", err)
	}
	version, err := pipeline.RiverVersion(ctx, pool)
	if err != nil {
		t.Fatalf("river version: %v", err)
	}
	if version == 0 {
		t.Fatal("river schema version is 0 after migrating up")
	}

	objects := newIntegrationStore(t, env)
	key := "uploads/preflight/" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".pdf"
	body := []byte("%PDF-1.7\npreflight\n%%EOF\n")
	t.Cleanup(func() { _ = objects.Delete(context.Background(), key) })
	if err := objects.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := objects.Get(ctx, key, 1<<20)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip returned %d bytes, want %d", len(got), len(body))
	}
	if err := objects.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Logf("postgres ok (app migrations %d, river schema %d); s3 bucket %q ok",
		len(applied), version, env.s3Bucket)
}

// newIntegrationStore builds the real path-style S3 client and proves the
// bucket is reachable and listable, so a bad endpoint or credential fails with
// a clear message instead of a confusing assertion later. The operator
// provisions the bucket.
func newIntegrationStore(t *testing.T, env integrationEnv) *storage.S3Store {
	t.Helper()
	objects, err := storage.NewS3Store(storage.S3Config{
		Endpoint: env.s3Endpoint, Region: env.s3Region, Bucket: env.s3Bucket,
		AccessKeyID: env.s3AccessKeyID, SecretAccessKey: env.s3SecretAccessKey,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 store: %v", err)
	}
	if _, err := objects.List(t.Context(), "uploads"); err != nil {
		t.Fatalf("bucket %q is not listable: %v", env.s3Bucket, err)
	}
	return objects
}

// harness wires the real store, River client and worker for one isolated
// schema and one S3 bucket.
type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	objects   *storage.S3Store
	validator *ir.Validator
	assembler *store.Assembler
	repo      *store.Repository
	adapter   *parser.MockParserAdapter
	submitter *pipeline.Submitter
	worker    *pipeline.IngestWorker
}

func newHarness(t *testing.T, opts ...parser.MockOption) *harness {
	t.Helper()
	env := loadIntegrationEnv(t)
	ctx := t.Context()
	pool := isolatedSchema(t, env)

	migrations, err := migrate.Load(examforge.Migrations)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("app migrations: %v", err)
	}
	if _, err := pipeline.RiverMigrate(ctx, pool, 0); err != nil {
		t.Fatalf("river migrations: %v", err)
	}
	if version, err := pipeline.RiverVersion(ctx, pool); err != nil || version == 0 {
		t.Fatalf("river schema version = %d, %v", version, err)
	}

	objects := newIntegrationStore(t, env)
	validator, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	assembler, err := store.NewAssembler(validator)
	if err != nil {
		t.Fatalf("assembler: %v", err)
	}
	repo := store.NewRepository(pool)
	persister, err := store.NewPersister(repo, assembler)
	if err != nil {
		t.Fatalf("persister: %v", err)
	}
	adapter, err := parser.NewMockParserAdapter(append([]parser.MockOption{parser.FromGolden(
		"../../testdata/golden/ir/sample.json", "../../testdata/golden/assets")}, opts...)...)
	if err != nil {
		t.Fatalf("mock parser: %v", err)
	}
	// Build the client first so the worker's cancel path reaches the real
	// River client instead of a fake.
	workers := river.NewWorkers()
	client, err := pipeline.NewRiverClient(pool, workers, &river.Config{
		Queues:            map[string]river.QueueConfig{"ingest": {MaxWorkers: 4}},
		FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	worker, err := pipeline.NewIngestWorker(repo, persister, objects, adapter, validator,
		pipeline.RiverClient{Client: client}, pipeline.WorkerConfig{})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if err := river.AddWorkerSafely(workers, worker); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start river client: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	submitter, err := pipeline.NewSubmitter(pipeline.PGXRunner{Pool: pool}, pipeline.RiverClient{Client: client}, objects, pipeline.SubmitConfig{})
	if err != nil {
		t.Fatalf("submitter: %v", err)
	}
	// Upload keys embed the job UUID, so a snapshot diff removes exactly this
	// test's sources on every exit path. Asset keys are content-addressed from
	// the fixture and therefore shared and idempotent by design; deleting them
	// would race a concurrent run, so they are left in place.
	before := listKeys(t, objects, "uploads")
	t.Cleanup(func() { deleteNewKeys(objects, "uploads", before) })

	return &harness{t: t, pool: pool, objects: objects, validator: validator,
		assembler: assembler, repo: repo, adapter: adapter, submitter: submitter, worker: worker}
}

// listKeys records the current keys under prefix.
func listKeys(t *testing.T, objects storage.ObjectLister, prefix string) map[string]bool {
	t.Helper()
	keys, err := objects.List(t.Context(), prefix)
	if err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	out := make(map[string]bool, len(keys))
	for _, obj := range keys {
		out[obj.Key] = true
	}
	return out
}

// deleteNewKeys removes every key under prefix that was not in the snapshot.
func deleteNewKeys(objects storage.ObjectStore, prefix string, before map[string]bool) {
	keys, err := objects.(storage.ObjectLister).List(context.Background(), prefix)
	if err != nil {
		return
	}
	for _, obj := range keys {
		if !before[obj.Key] {
			_ = objects.Delete(context.Background(), obj.Key)
		}
	}
}

// submit uploads source and returns the queued job. The harness cleanup
// removes every upload object this test created; content-addressed assets are
// immutable and shared, so they are left in place.
func (h *harness) submit(source []byte) pipeline.Submission {
	h.t.Helper()
	sub, err := h.submitter.Submit(h.t.Context(), bytes.NewReader(source), int64(len(source)))
	if err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	return sub
}

func (h *harness) waitState(jobID uuid.UUID, want string) {
	h.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		row, err := h.repo.GetJob(h.t.Context(), toPG(jobID))
		if err != nil {
			h.t.Fatalf("get job: %v", err)
		}
		last = row.State
		if row.State == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.dumpJobDiagnostics(jobID, last, want)
	h.t.Fatalf("job %s stayed in state %q, want %q", jobID, last, want)
}

// dumpJobDiagnostics prints the domain row and every River attempt error so a
// stuck job names its cause instead of only its state.
func (h *harness) dumpJobDiagnostics(jobID uuid.UUID, last, want string) {
	h.t.Helper()
	row, err := h.repo.GetJob(h.t.Context(), toPG(jobID))
	if err != nil {
		h.t.Logf("diagnostics: read job: %v", err)
		return
	}
	h.t.Logf("diagnostics: job state=%s want=%s attempt=%d code=%v message=%v",
		last, want, row.AttemptCount, row.ErrorCode.String, row.ErrorMessage.String)
	if !row.RiverJobID.Valid {
		h.t.Logf("diagnostics: job has no river_job_id")
		return
	}
	rows, err := h.pool.Query(h.t.Context(),
		"SELECT attempt, state, errors::text FROM river_job WHERE id = $1::bigint", row.RiverJobID.String)
	if err != nil {
		h.t.Logf("diagnostics: read river job: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			attempt int
			state   string
			errs    string
		)
		if err := rows.Scan(&attempt, &state, &errs); err != nil {
			h.t.Logf("diagnostics: scan river job: %v", err)
			return
		}
		h.t.Logf("diagnostics: river job attempt=%d state=%s errors=%s", attempt, state, errs)
	}
	if err := rows.Err(); err != nil {
		h.t.Logf("diagnostics: river job rows: %v", err)
	}
}

func (h *harness) riverState(id int64) rivertype.JobState {
	h.t.Helper()
	var state rivertype.JobState
	if err := h.pool.QueryRow(h.t.Context(), "SELECT state FROM river_job WHERE id = $1", id).Scan(&state); err != nil {
		h.t.Fatalf("river job %d: %v", id, err)
	}
	return state
}

// waitRiverState polls until the River job reaches want. The domain row is
// written inside the worker, before River finalizes the attempt, so a check
// immediately after the domain state would race.
func (h *harness) waitRiverState(id int64, want rivertype.JobState) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last rivertype.JobState
	for time.Now().Before(deadline) {
		last = h.riverState(id)
		if last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("river job %d stayed in state %q, want %q", id, last, want)
}

func (h *harness) examID(jobID uuid.UUID) uuid.UUID {
	h.t.Helper()
	row, err := h.repo.GetJob(h.t.Context(), toPG(jobID))
	if err != nil {
		h.t.Fatalf("get job: %v", err)
	}
	if !row.ExamID.Valid {
		h.t.Fatalf("job %s has no exam", jobID)
	}
	return uuid.UUID(row.ExamID.Bytes)
}

func (h *harness) assemble(examID uuid.UUID) ir.ExamDocumentIR {
	h.t.Helper()
	var exam ir.ExamDocumentIR
	if err := h.repo.Read(h.t.Context(), func(ctx context.Context, rd store.Reader) error {
		var err error
		exam, err = h.assembler.Assemble(ctx, rd, examID)
		return err
	}); err != nil {
		h.t.Fatalf("assemble: %v", err)
	}
	if err := h.validator.ValidateExamDTO(exam); err != nil {
		h.t.Fatalf("assembled exam is invalid: %v", err)
	}
	return exam
}

// TestIntegrationFullPipeline is spec §11.3 item 1: real PostgreSQL and River
// migrations, a path-style S3 upload, one transactional job + River enqueue,
// a real River worker consuming the job, persistence, assembly, validation and
// a Typst render through the S3 resolver.
func TestIntegrationFullPipeline(t *testing.T) {
	if _, err := exec.LookPath("typst"); err != nil {
		t.Fatalf("typst is required for the integration gate: %v", err)
	}
	h := newHarness(t)
	source := []byte("%PDF-1.7\nintegration source\n%%EOF\n")
	sub := h.submit(source)

	var linked string
	if err := h.pool.QueryRow(t.Context(), "SELECT river_job_id FROM ingest_jobs WHERE job_id = $1", toPG(sub.JobID)).Scan(&linked); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if linked != fmt.Sprint(sub.RiverJobID) {
		t.Errorf("river_job_id = %q, want %d", linked, sub.RiverJobID)
	}

	h.waitState(sub.JobID, store.JobPersisted)
	h.waitRiverState(sub.RiverJobID, rivertype.JobStateCompleted)

	exam := h.assemble(h.examID(sub.JobID))

	resolver, err := storage.NewS3AssetResolver(h.objects, h.repo, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	// One render scope, so a repeated asset ID is downloaded once.
	scope := resolver.ForRender()
	for id, asset := range exam.Assets {
		for attempt := 0; attempt < 2; attempt++ {
			rc, err := scope.Resolve(t.Context(), id, asset)
			if err != nil {
				t.Fatalf("resolve %s (attempt %d): %v", id, attempt, err)
			}
			if _, err := io.ReadAll(rc); err != nil {
				rc.Close()
				t.Fatalf("read %s: %v", id, err)
			}
			rc.Close()
		}
	}

	layoutData, err := os.ReadFile("../../testdata/golden/layout/student.json")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := h.validator.DecodeLayoutSpec(bytes.NewReader(layoutData))
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{
		SchemaDir:     "../../schemas",
		TemplateDir:   filepath.Join("..", "..", "templates"),
		PackageDir:    filepath.Join("..", "..", "third_party", "typst-packages"),
		FontDir:       filepath.Join("..", "..", "third_party", "fonts"),
		Validator:     h.validator,
		AssetResolver: resolver.ForRender(),
	})
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	artifact, err := renderer.Render(t.Context(), exam, layout)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(artifact.Bytes, []byte("%PDF-")) {
		t.Errorf("artifact does not start with %%PDF-")
	}

	// A duplicate delivery of a finished job is a no-op, even through the
	// worker seam directly.
	job := &river.Job[pipeline.IngestArgs]{
		JobRow: &rivertype.JobRow{ID: sub.RiverJobID, Kind: pipeline.IngestArgs{}.Kind(), Attempt: 1},
		Args:   pipeline.IngestArgs{JobID: sub.JobID.String()},
	}
	if err := h.worker.Work(t.Context(), job); err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertCount(t, h.pool, "SELECT count(*) FROM exams", 1)
}

// TestIntegrationSubmissionRollback is spec §11.3 item 2: a failure after the
// enqueue leaves neither an ingest_jobs row nor a River job.
func TestIntegrationSubmissionRollback(t *testing.T) {
	env := loadIntegrationEnv(t)
	ctx := t.Context()
	pool := isolatedSchema(t, env)
	migrations, err := migrate.Load(examforge.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(ctx, pool, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RiverMigrate(ctx, pool, 0); err != nil {
		t.Fatal(err)
	}
	objects := newIntegrationStore(t, env)
	before := listKeys(t, objects, "uploads")
	t.Cleanup(func() { deleteNewKeys(objects, "uploads", before) })

	client, err := pipeline.NewRiverClient(pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	submitter, err := pipeline.NewSubmitter(failingRunner{pool: pool}, pipeline.RiverClient{Client: client}, objects, pipeline.SubmitConfig{})
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("%PDF-1.7\nrollback\n%%EOF\n")
	if _, err := submitter.Submit(ctx, bytes.NewReader(source), int64(len(source))); err == nil {
		t.Fatal("submit succeeded although the transaction failed after the enqueue")
	}
	assertCount(t, pool, "SELECT count(*) FROM ingest_jobs", 0)
	assertCount(t, pool, "SELECT count(*) FROM river_job", 0)
}

// TestIntegrationDuplicateSourceJobsBothPersist is spec §11.3 item 3:
// source_file_sha256 is not unique, so two jobs may parse the same bytes and
// each produce its own exam.
func TestIntegrationDuplicateSourceJobsBothPersist(t *testing.T) {
	h := newHarness(t)
	source := []byte("%PDF-1.7\nsame source\n%%EOF\n")
	first := h.submit(source)
	second := h.submit(source)
	h.waitState(first.JobID, store.JobPersisted)
	h.waitState(second.JobID, store.JobPersisted)

	if first.JobID == second.JobID || first.SourceSHA256 != second.SourceSHA256 {
		t.Fatalf("expected two jobs with one source digest, got %+v and %+v", first, second)
	}
	if h.examID(first.JobID) == h.examID(second.JobID) {
		t.Error("two jobs from equal source bytes shared one exam")
	}
	assertCount(t, h.pool, "SELECT count(*) FROM exams", 2)
	assertCount(t, h.pool, "SELECT count(*) FROM exams WHERE source_file_sha256 = '"+first.SourceSHA256+"'", 2)
	assertCount(t, h.pool, "SELECT count(DISTINCT source_pdf_key) FROM exams", 2)
}

// TestIntegrationSharedQuestions is spec §11.3 item 5: two exams from the same
// golden content share question entities, and per-exam ordering is preserved.
func TestIntegrationSharedQuestions(t *testing.T) {
	h := newHarness(t)
	first := h.submit([]byte("%PDF-1.7\nfirst\n%%EOF\n"))
	h.waitState(first.JobID, store.JobPersisted)
	second := h.submit([]byte("%PDF-1.7\nsecond\n%%EOF\n"))
	h.waitState(second.JobID, store.JobPersisted)

	firstExam, secondExam := h.examID(first.JobID), h.examID(second.JobID)
	if firstExam == secondExam {
		t.Fatal("two jobs produced one exam")
	}
	assertCount(t, h.pool, "SELECT count(*) FROM questions", 3)
	assertCount(t, h.pool, "SELECT count(*) FROM exam_items", 6)
	assertCount(t, h.pool, "SELECT count(DISTINCT question_id) FROM exam_items", 3)

	ids := func(examID uuid.UUID) []string {
		rows, err := h.pool.Query(h.t.Context(), `
			SELECT i.question_id::text
			FROM exam_items i JOIN exam_sections s USING (section_id)
			WHERE s.exam_id = $1 ORDER BY s.ordinal, i.ordinal`, toPG(examID))
		if err != nil {
			h.t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				h.t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	a, b := ids(firstExam), ids(secondExam)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("question entity order differs between exams:\n first:  %v\n second: %v", a, b)
	}
	for _, id := range a {
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("question id %q is not a UUID", id)
		}
	}
}

// TestIntegrationTransientRetry is spec §11.3 item 6 (transient half): a
// parser failure is retried by River and the second attempt persists.
func TestIntegrationTransientRetry(t *testing.T) {
	h := newHarness(t, parser.FailTimes(1, errors.New("simulated upstream timeout")))
	sub := h.submit([]byte("%PDF-1.7\ntransient\n%%EOF\n"))
	h.waitState(sub.JobID, store.JobPersisted)
	if calls := h.adapter.Calls(); calls < 2 {
		t.Errorf("parser calls = %d, want at least 2", calls)
	}
	h.waitRiverState(sub.RiverJobID, rivertype.JobStateCompleted)
}

// TestIntegrationPermanentFailureCancels is spec §11.3 item 6 (permanent
// half): an invalid candidate IR fails the domain row and cancels the River
// job, which is never retried.
func TestIntegrationPermanentFailureCancels(t *testing.T) {
	h := newHarness(t, parser.InvalidTimes(1))
	sub := h.submit([]byte("%PDF-1.7\ninvalid\n%%EOF\n"))
	h.waitState(sub.JobID, store.JobFailed)
	if calls := h.adapter.Calls(); calls != 1 {
		t.Errorf("parser calls = %d, want 1", calls)
	}
	h.waitRiverState(sub.RiverJobID, rivertype.JobStateCancelled)
	row, err := h.repo.GetJob(t.Context(), toPG(sub.JobID))
	if err != nil {
		t.Fatal(err)
	}
	if !row.ErrorCode.Valid || row.ErrorCode.String != pipeline.CodeIRInvalid {
		t.Errorf("error code = %v, want %s", row.ErrorCode, pipeline.CodeIRInvalid)
	}
	assertCount(t, h.pool, "SELECT count(*) FROM exams", 0)
}

// TestIntegrationAssetImmutability is spec §11.3 item 4: replaying equal bytes
// is idempotent; different bytes are rejected and the stored object is kept.
func TestIntegrationAssetImmutability(t *testing.T) {
	env := loadIntegrationEnv(t)
	ctx := t.Context()
	objects := newIntegrationStore(t, env)
	assetID := "ast_integration" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	key, err := storage.RawAssetKey(assetID, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Delete(context.Background(), key) })

	data := []byte("\x89PNG\r\n\x1a\nintegration")
	if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "image/png"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "image/png"); err != nil {
		t.Errorf("idempotent put: %v", err)
	}
	other := []byte("\x89PNG\r\n\x1a\nother")
	if err := objects.Put(ctx, key, bytes.NewReader(other), int64(len(other)), "image/png"); !errors.Is(err, storage.ErrConflict) {
		t.Errorf("conflicting put err = %v, want ErrConflict", err)
	}
	info, err := objects.Stat(ctx, key)
	if err != nil || info.Size != int64(len(data)) {
		t.Errorf("stored object = %+v, %v", info, err)
	}
}

// failingRunner commits the enqueue, then fails, to prove the rollback covers
// both writes.
type failingRunner struct{ pool *pgxpool.Pool }

func (r failingRunner) Run(ctx context.Context, fn func(ctx context.Context, scope pipeline.TxScope) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx, integrationScope{tx: tx}); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	// Injected failure after every step succeeded inside the transaction.
	_ = tx.Rollback(ctx)
	return errors.New("injected failure after enqueue")
}

type integrationScope struct{ tx pgx.Tx }

func (s integrationScope) Jobs() store.IngestJobs { return store.WorkspaceTx(s.tx) }
func (s integrationScope) Raw() pgx.Tx            { return s.tx }

func toPG(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, query string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got != want {
		t.Errorf("%s = %d, want %d", query, got, want)
	}
}
