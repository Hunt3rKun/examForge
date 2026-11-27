package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
)

// IngestArgs is the queued job payload. It carries only the job ID: every
// other input is read from the ingest_jobs row, so a retried job sees the
// same source.
type IngestArgs struct {
	JobID string `json:"job_id"`
}

// Kind implements river.JobArgs.
func (IngestArgs) Kind() string { return "examforge_ingest" }

// TxScope is what a submission transaction can use: the job queries bound to
// the transaction and the raw transaction River inserts into.
type TxScope interface {
	Jobs() store.IngestJobs
	Raw() pgx.Tx
}

// TxRunner runs fn inside one database transaction.
type TxRunner interface {
	Run(ctx context.Context, fn func(ctx context.Context, scope TxScope) error) error
}

// Enqueuer inserts a River job inside an existing transaction.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// JobCanceller cancels one River job by ID.
type JobCanceller interface {
	JobCancel(ctx context.Context, jobID int64) (*rivertype.JobRow, error)
}

// uuidToPG converts a UUID to the pgx wire type.
func uuidToPG(id uuid.UUID) pgtype.UUID {
	var out pgtype.UUID
	copy(out.Bytes[:], id[:])
	out.Valid = true
	return out
}

// Submission reports one accepted upload.
type Submission struct {
	JobID        uuid.UUID
	SourceKey    string
	SourceSHA256 string
	RiverJobID   int64
}

// Submitter uploads a source PDF and queues its ingest job atomically: the
// ingest_jobs row and the River job commit together or not at all.
type Submitter struct {
	runner      TxRunner
	enqueuer    Enqueuer
	store       storage.ObjectStore
	queue       string
	maxAttempts int
}

// SubmitConfig configures a Submitter.
type SubmitConfig struct {
	// Queue is the River queue name. Default: "ingest".
	Queue string
	// MaxAttempts is the River attempt limit. Default: 5.
	MaxAttempts int
}

// NewSubmitter wires the submitter.
func NewSubmitter(runner TxRunner, enqueuer Enqueuer, objects storage.ObjectStore, cfg SubmitConfig) (*Submitter, error) {
	if runner == nil || enqueuer == nil || objects == nil {
		return nil, errors.New("pipeline: submitter needs a tx runner, an enqueuer and an object store")
	}
	if cfg.Queue == "" {
		cfg.Queue = "ingest"
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.MaxAttempts < 0 {
		return nil, errors.New("pipeline: MaxAttempts must not be negative")
	}
	return &Submitter{runner: runner, enqueuer: enqueuer, store: objects, queue: cfg.Queue, maxAttempts: cfg.MaxAttempts}, nil
}

// Submit reads the whole source (bounded), verifies the PDF signature, uploads
// it under the deterministic job key, then inserts the job row and the River
// job in one transaction. A failure after the upload leaves an orphan under
// uploads/{job_id}/, which is safe to delete: the job row never exists.
func (s *Submitter) Submit(ctx context.Context, source io.Reader, size int64) (Submission, error) {
	if size < 0 || size > storage.MaxSourceBytes {
		return Submission{}, Permanent(CodeSourceInvalid, fmt.Errorf("source size %d is outside 1..%d", size, storage.MaxSourceBytes))
	}
	data, err := io.ReadAll(io.LimitReader(source, storage.MaxSourceBytes+1))
	if err != nil {
		return Submission{}, fmt.Errorf("read source: %w", err)
	}
	if int64(len(data)) != size {
		return Submission{}, Permanent(CodeSourceInvalid, fmt.Errorf("source is %d bytes but declared %d", len(data), size))
	}
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		return Submission{}, Permanent(CodeSourceInvalid, errors.New("source is not a PDF"))
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	jobID := uuid.New()
	key := storage.SourceKey(jobID)
	if err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/pdf"); err != nil {
		return Submission{}, fmt.Errorf("upload source: %w", err)
	}

	var riverJobID int64
	err = s.runner.Run(ctx, func(ctx context.Context, scope TxScope) error {
		job, err := scope.Jobs().InsertIngestJob(ctx, sqlcgen.InsertIngestJobParams{
			JobID:            uuidToPG(jobID),
			SourcePdfKey:     key,
			SourceFileSha256: sha,
		})
		if err != nil {
			return fmt.Errorf("insert ingest job: %w", err)
		}
		if job.State != store.JobQueued {
			return fmt.Errorf("insert ingest job: state %q", job.State)
		}
		result, err := s.enqueuer.InsertTx(ctx, scope.Raw(), IngestArgs{JobID: jobID.String()}, &river.InsertOpts{
			Queue:       s.queue,
			MaxAttempts: s.maxAttempts,
		})
		if err != nil {
			return fmt.Errorf("enqueue river job: %w", err)
		}
		riverJobID = result.Job.ID
		if _, err := scope.Jobs().SetRiverJobID(ctx, sqlcgen.SetRiverJobIDParams{
			JobID:      uuidToPG(jobID),
			RiverJobID: pgtype.Text{String: fmt.Sprint(riverJobID), Valid: true},
		}); err != nil {
			return fmt.Errorf("link river job: %w", err)
		}
		return nil
	})
	if err != nil {
		return Submission{}, err
	}
	return Submission{JobID: jobID, SourceKey: key, SourceSHA256: sha, RiverJobID: riverJobID}, nil
}
