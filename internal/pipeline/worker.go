package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"golang.org/x/sync/errgroup"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
)

// JobStore is the job bookkeeping the worker needs. *store.Repository
// satisfies it; a test supplies an in-memory implementation.
type JobStore interface {
	GetJob(ctx context.Context, id pgtype.UUID) (sqlcgen.IngestJob, error)
	Claim(ctx context.Context, id pgtype.UUID, from []store.JobState) (sqlcgen.IngestJob, error)
	Move(ctx context.Context, id pgtype.UUID, to store.JobState, from []store.JobState) (sqlcgen.IngestJob, error)
	Fail(ctx context.Context, id pgtype.UUID, from []store.JobState, code, message string) (sqlcgen.IngestJob, error)
}

// Persistence runs the one persistence transaction: it writes the exam and
// completes the job together.
type Persistence interface {
	Persist(ctx context.Context, exam ir.ExamDocumentIR, source store.SourceRecord) (store.ExamRecord, error)
}

// WorkerConfig configures an IngestWorker.
type WorkerConfig struct {
	// MaxSourceBytes bounds the source read. Default: storage.MaxSourceBytes.
	MaxSourceBytes int64
	// MaxAssetBytes bounds one asset. Default: storage.MaxAssetBytes.
	MaxAssetBytes int64
	// UploadConcurrency bounds parallel asset uploads. Default: 4.
	UploadConcurrency int
	// Secrets are scrubbed from every stored error message.
	Secrets []string
}

// IngestWorker is the River worker for IngestArgs. It never holds a database
// transaction across an S3 read or the parser call.
type IngestWorker struct {
	river.WorkerDefaults[IngestArgs]

	jobs        JobStore
	persistence Persistence
	objects     storage.ObjectStore
	parser      parser.ParserAdapter
	validator   *ir.Validator
	canceller   JobCanceller
	cfg         WorkerConfig
}

var _ river.Worker[IngestArgs] = (*IngestWorker)(nil)

// NewIngestWorker wires the worker.
func NewIngestWorker(jobs JobStore, persistence Persistence, objects storage.ObjectStore,
	adapter parser.ParserAdapter, validator *ir.Validator, canceller JobCanceller, cfg WorkerConfig) (*IngestWorker, error) {
	switch {
	case jobs == nil || persistence == nil || objects == nil || adapter == nil || validator == nil:
		return nil, errors.New("pipeline: worker needs a job store, persistence, object store, parser and validator")
	case cfg.MaxSourceBytes < 0 || cfg.MaxAssetBytes < 0 || cfg.UploadConcurrency < 0:
		return nil, errors.New("pipeline: worker limits must not be negative")
	}
	if cfg.MaxSourceBytes == 0 {
		cfg.MaxSourceBytes = storage.MaxSourceBytes
	}
	if cfg.MaxAssetBytes == 0 {
		cfg.MaxAssetBytes = storage.MaxAssetBytes
	}
	if cfg.UploadConcurrency == 0 {
		cfg.UploadConcurrency = 4
	}
	return &IngestWorker{jobs: jobs, persistence: persistence, objects: objects, parser: adapter,
		validator: validator, canceller: canceller, cfg: cfg}, nil
}

// Work runs one attempt.
//
// Permanent failures are recorded on the job (state failed) and returned as a
// River cancel, so River never retries them. Transient failures are returned
// as ordinary errors: River's backoff retries them and the domain row keeps
// its active state.
func (w *IngestWorker) Work(ctx context.Context, job *river.Job[IngestArgs]) error {
	jobID, err := uuid.Parse(job.Args.JobID)
	if err != nil {
		// Nothing can be recorded: the arguments do not name a job row.
		return river.JobCancel(Permanent(CodeMalformedArgs, err))
	}
	row, err := w.jobs.GetJob(ctx, uuidToPG(jobID))
	if err != nil {
		return w.classify(ctx, job, jobID, err)
	}
	switch row.State {
	case store.JobPersisted:
		// A replayed or duplicate delivery of a finished job: idempotent.
		return nil
	case store.JobFailed:
		// The outcome is already final; River must stop, not retry.
		return river.JobCancel(fmt.Errorf("ingest job %s already failed: %s", jobID, row.ErrorMessage.String))
	}

	// Claim: queued -> parsing, or re-entry into parsing after an attempt
	// that died in an active state.
	claimed, err := w.jobs.Claim(ctx, uuidToPG(jobID), ClaimStates)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			again, readErr := w.jobs.GetJob(ctx, uuidToPG(jobID))
			if readErr != nil {
				return w.classify(ctx, job, jobID, readErr)
			}
			if again.State == store.JobPersisted {
				return nil
			}
			return river.JobCancel(fmt.Errorf("ingest job %s is in state %s", jobID, again.State))
		}
		return w.classify(ctx, job, jobID, fmt.Errorf("claim job: %w", err))
	}

	if _, err := w.ingest(ctx, claimed); err != nil {
		return w.classify(ctx, job, jobID, err)
	}
	return nil
}

// ingest is the happy path: source -> parser -> validation -> uploads ->
// persistence.
func (w *IngestWorker) ingest(ctx context.Context, job sqlcgen.IngestJob) (store.ExamRecord, error) {
	jobID := uuid.UUID(job.JobID.Bytes)
	source := parser.Source{
		JobID:      jobID,
		StorageKey: job.SourcePdfKey,
		SHA256:     job.SourceFileSha256,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			return w.objects.Get(ctx, job.SourcePdfKey, w.cfg.MaxSourceBytes)
		},
	}
	result, err := w.parser.Parse(ctx, source)
	if err != nil {
		return store.ExamRecord{}, fmt.Errorf("parse: %w", err)
	}
	// Full validation before any upload: schema and semantic rules.
	if err := w.validator.ValidateExamDTO(result.Exam); err != nil {
		return store.ExamRecord{}, Permanent(CodeIRInvalid, err)
	}
	if err := w.move(ctx, jobID, store.JobValidating, store.JobParsing); err != nil {
		return store.ExamRecord{}, err
	}
	assets, err := w.uploadAssets(ctx, result)
	if err != nil {
		return store.ExamRecord{}, err
	}
	record := store.SourceRecord{
		JobID:        jobID,
		SourcePDFKey: job.SourcePdfKey,
		SourceSHA256: job.SourceFileSha256,
		Assets:       assets,
	}
	persisted, err := w.persistence.Persist(ctx, result.Exam, record)
	if err != nil {
		return store.ExamRecord{}, fmt.Errorf("persist: %w", err)
	}
	return persisted, nil
}

// uploadAssets verifies and uploads every asset with bounded concurrency and
// returns the verified upload records in asset-ID order. A failure before the
// persistence transaction leaves only objects under deterministic keys, which
// a retry verifies by digest instead of rewriting.
func (w *IngestWorker) uploadAssets(ctx context.Context, result parser.ParseResult) ([]store.VerifiedAsset, error) {
	declared := make(map[string]parser.AssetBlob, len(result.Assets))
	for _, blob := range result.Assets {
		if _, dup := declared[blob.AssetID]; dup {
			return nil, Permanent(CodeAssetInvalid, fmt.Errorf("asset %q is declared twice", blob.AssetID))
		}
		declared[blob.AssetID] = blob
	}
	for id := range declared {
		if _, ok := result.Exam.Assets[id]; !ok {
			return nil, Permanent(CodeAssetInvalid, fmt.Errorf("asset %q has bytes but is not part of the exam", id))
		}
	}
	ids := make([]string, 0, len(result.Exam.Assets))
	for id := range result.Exam.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	verified := make([]store.VerifiedAsset, len(ids))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(w.cfg.UploadConcurrency)
	for i, id := range ids {
		i, id := i, id
		asset := result.Exam.Assets[id]
		group.Go(func() error {
			blob, ok := declared[id]
			if !ok {
				return Permanent(CodeAssetInvalid, fmt.Errorf("asset %q has no bytes", id))
			}
			upload, err := w.uploadOne(groupCtx, id, asset.MediaType, blob)
			if err != nil {
				return err
			}
			verified[i] = upload
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return verified, nil
}

func (w *IngestWorker) uploadOne(ctx context.Context, id, mediaType string, blob parser.AssetBlob) (store.VerifiedAsset, error) {
	if err := storage.ValidateAssetID(id); err != nil {
		return store.VerifiedAsset{}, Permanent(CodeAssetInvalid, err)
	}
	if len(blob.Data) == 0 || int64(len(blob.Data)) > w.cfg.MaxAssetBytes {
		return store.VerifiedAsset{}, Permanent(CodeAssetInvalid, fmt.Errorf("asset %q is %d bytes; limit %d", id, len(blob.Data), w.cfg.MaxAssetBytes))
	}
	sum := storage.HashBytes(blob.Data)
	if blob.SHA256 != "" && blob.SHA256 != sum {
		return store.VerifiedAsset{}, Permanent(CodeAssetInvalid, fmt.Errorf("asset %q declares SHA-256 %s but its bytes hash to %s", id, blob.SHA256, sum))
	}
	if err := validateAssetBytes(mediaType, blob.Data); err != nil {
		return store.VerifiedAsset{}, Permanent(CodeAssetInvalid, fmt.Errorf("asset %q: %w", id, err))
	}
	key, err := storage.RawAssetKey(id, mediaType)
	if err != nil {
		return store.VerifiedAsset{}, Permanent(CodeAssetInvalid, err)
	}
	got, err := storage.PutImmutable(ctx, w.objects, key, bytes.NewReader(blob.Data), int64(len(blob.Data)), mediaType, w.cfg.MaxAssetBytes)
	if err != nil {
		return store.VerifiedAsset{}, err
	}
	return store.VerifiedAsset{AssetID: id, StorageKey: key, SHA256: got}, nil
}

// move performs one conditional state update and turns a no-match into an
// explicit error.
func (w *IngestWorker) move(ctx context.Context, jobID uuid.UUID, to, from store.JobState) error {
	if err := CheckTransition(from, to); err != nil {
		return Permanent(CodeStoreInvalid, err)
	}
	if _, err := w.jobs.Move(ctx, uuidToPG(jobID), to, []store.JobState{from}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Permanent(CodeStoreInvalid, fmt.Errorf("job %s is not in state %s", jobID, from))
		}
		return fmt.Errorf("move job %s to %s: %w", jobID, to, err)
	}
	return nil
}

// classify records the outcome of a failed attempt. A permanent failure marks
// the job failed and cancels the River job; a transient one only returns the
// error so River retries.
func (w *IngestWorker) classify(ctx context.Context, job *river.Job[IngestArgs], jobID uuid.UUID, err error) error {
	if ctx.Err() != nil && !errors.Is(err, context.Canceled) {
		// Shutdown or deadline: let River retry later; not an input defect.
		return err
	}
	code, permanent := Classify(err, CodeTransient)
	if !permanent {
		return err
	}
	if jobID != uuid.Nil {
		message := storage.Redact(err.Error(), MaxErrorMessageRunes, w.cfg.Secrets...)
		if _, failErr := w.jobs.Fail(ctx, uuidToPG(jobID), ClaimStates, code, message); failErr != nil && !errors.Is(failErr, store.ErrNotFound) {
			// The job could not be marked failed. Return a transient error so
			// River retries and the terminal-state sync happens then, instead
			// of losing the outcome.
			return fmt.Errorf("record permanent failure: %w (original: %v)", failErr, err)
		}
	}
	// The domain row is final. Cancelling the River job stops its retries.
	if w.canceller != nil && job != nil {
		if _, cancelErr := w.canceller.JobCancel(ctx, job.ID); cancelErr != nil && !isCancelError(cancelErr) {
			return fmt.Errorf("cancel river job %d: %w (original: %v)", job.ID, cancelErr, err)
		}
	}
	return river.JobCancel(err)
}

func isCancelError(err error) bool {
	var cancelErr *rivertype.JobCancelError
	return errors.As(err, &cancelErr)
}
