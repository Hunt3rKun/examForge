// Package store is the persistence boundary: the ExamAssembler maps Exam IR
// to and from the assets/questions/exams schema, and Repository opens the
// transaction-scoped Workspace the assembler and the ingest pipeline use.
//
// The IR stays an exchange DTO. Row types come from sqlc (sqlcgen) and are
// never returned to render or exchange callers.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"examforge/internal/storage"
	"examforge/internal/store/sqlcgen"
)

// Store error classes. Invalid input and conflicts are permanent; anything
// else that is not ErrNotFound should be treated as transient by callers.
var (
	ErrNotFound = errors.New("store: record not found")
	ErrConflict = errors.New("store: conflicting record")
	ErrInvalid  = errors.New("store: invalid record")
)

// Reader is the read side used by Assemble and the API services.
// *sqlcgen.Queries satisfies it.
type Reader interface {
	GetExam(ctx context.Context, examID pgtype.UUID) (sqlcgen.Exam, error)
	ListSections(ctx context.Context, examID pgtype.UUID) ([]sqlcgen.ExamSection, error)
	ListItems(ctx context.Context, sectionID pgtype.UUID) ([]sqlcgen.ExamItem, error)
	ListExamItems(ctx context.Context, examID pgtype.UUID) ([]sqlcgen.ExamItem, error)
	ListQuestions(ctx context.Context, question_ids []pgtype.UUID) ([]sqlcgen.Question, error)
	ListAssets(ctx context.Context, asset_ids []string) ([]sqlcgen.Asset, error)
	GetAsset(ctx context.Context, assetID string) (sqlcgen.Asset, error)
	GetIngestJob(ctx context.Context, jobID pgtype.UUID) (sqlcgen.IngestJob, error)
	ListQuestionReferenceCounts(ctx context.Context, question_ids []pgtype.UUID) ([]sqlcgen.ListQuestionReferenceCountsRow, error)
	ListExamsPage(ctx context.Context, arg sqlcgen.ListExamsPageParams) ([]sqlcgen.Exam, error)
	ListIngestJobsPage(ctx context.Context, arg sqlcgen.ListIngestJobsPageParams) ([]sqlcgen.IngestJob, error)
}

// Workspace is the transaction-scoped data access the assembler and the edit
// service need. *sqlcgen.Queries bound to a transaction satisfies it; every
// method of one Persist, Assemble or Edit call must share the same
// transaction.
//
// Lock protocol: every path that adds or moves an exam_items reference takes
// the referenced question rows with LockQuestions (sorted question_id order)
// before writing the reference, so a concurrent edit's reference count can
// never miss an attach. Exam and item locks follow exam -> exam_item ->
// question, the one order every writer uses.
type Workspace interface {
	Reader
	InsertAsset(ctx context.Context, arg sqlcgen.InsertAssetParams) (sqlcgen.Asset, error)
	UpdateAssetDisplay(ctx context.Context, arg sqlcgen.UpdateAssetDisplayParams) (int64, error)
	GetAssetForUpdate(ctx context.Context, assetID string) (sqlcgen.Asset, error)
	SetAssetActiveVariant(ctx context.Context, arg sqlcgen.SetAssetActiveVariantParams) (sqlcgen.Asset, error)
	ListQuestionsByFingerprint(ctx context.Context, arg sqlcgen.ListQuestionsByFingerprintParams) ([]sqlcgen.Question, error)
	ListQuestionsByFingerprintForUpdate(ctx context.Context, arg sqlcgen.ListQuestionsByFingerprintForUpdateParams) ([]sqlcgen.Question, error)
	LockQuestions(ctx context.Context, question_ids []pgtype.UUID) ([]pgtype.UUID, error)
	GetQuestionForUpdate(ctx context.Context, questionID pgtype.UUID) (sqlcgen.Question, error)
	InsertQuestion(ctx context.Context, arg sqlcgen.InsertQuestionParams) (sqlcgen.Question, error)
	UpdateQuestion(ctx context.Context, arg sqlcgen.UpdateQuestionParams) (sqlcgen.Question, error)
	DeleteQuestionIfUnreferenced(ctx context.Context, questionID pgtype.UUID) (int64, error)
	GetExamByJob(ctx context.Context, ingestJobID pgtype.UUID) (sqlcgen.Exam, error)
	GetExamForUpdate(ctx context.Context, examID pgtype.UUID) (sqlcgen.Exam, error)
	InsertExam(ctx context.Context, arg sqlcgen.InsertExamParams) (sqlcgen.Exam, error)
	UpdateExam(ctx context.Context, arg sqlcgen.UpdateExamParams) (sqlcgen.Exam, error)
	DeleteSections(ctx context.Context, examID pgtype.UUID) (int64, error)
	InsertSection(ctx context.Context, arg sqlcgen.InsertSectionParams) (sqlcgen.ExamSection, error)
	InsertItem(ctx context.Context, arg sqlcgen.InsertItemParams) error
	GetExamItemForUpdate(ctx context.Context, itemID pgtype.UUID) (sqlcgen.GetExamItemForUpdateRow, error)
	CountQuestionReferences(ctx context.Context, questionID pgtype.UUID) (int64, error)
	UpdateItem(ctx context.Context, arg sqlcgen.UpdateItemParams) (sqlcgen.ExamItem, error)
	CompleteIngestJob(ctx context.Context, arg sqlcgen.CompleteIngestJobParams) (sqlcgen.IngestJob, error)
}

// JobState is the domain state of an ingest job. It mirrors the state CHECK
// constraint; the transition rules live in internal/pipeline.
type JobState = string

// Ingest job states. There is no separate "retrying" state: River owns the
// retry schedule and a retried job re-enters the active states.
const (
	JobQueued     JobState = "queued"
	JobParsing    JobState = "parsing"
	JobValidating JobState = "validating"
	JobPersisted  JobState = "persisted"
	JobFailed     JobState = "failed"
)

// IngestJobs is the job bookkeeping every ingest job state change goes
// through. *sqlcgen.Queries satisfies it. Conditional updates that match no
// row report ErrNotFound: the caller distinguishes a missing job from an
// illegal transition by reading the job.
type IngestJobs interface {
	InsertIngestJob(ctx context.Context, arg sqlcgen.InsertIngestJobParams) (sqlcgen.IngestJob, error)
	SetRiverJobID(ctx context.Context, arg sqlcgen.SetRiverJobIDParams) (int64, error)
	GetIngestJob(ctx context.Context, jobID pgtype.UUID) (sqlcgen.IngestJob, error)
	ClaimIngestJob(ctx context.Context, arg sqlcgen.ClaimIngestJobParams) (sqlcgen.IngestJob, error)
	MoveIngestJob(ctx context.Context, arg sqlcgen.MoveIngestJobParams) (sqlcgen.IngestJob, error)
	FailIngestJob(ctx context.Context, arg sqlcgen.FailIngestJobParams) (sqlcgen.IngestJob, error)
	DeleteIngestJob(ctx context.Context, jobID pgtype.UUID) (int64, error)
}

// Translate maps driver errors to the store classes. Call it at every store
// boundary so callers can classify with errors.Is.
func Translate(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		return fmt.Errorf("%w: unique constraint %s", ErrConflict, pgErr.ConstraintName)
	case "23503":
		return fmt.Errorf("%w: reference %s does not exist", ErrConflict, pgErr.ConstraintName)
	case "23502":
		// A NOT NULL violation is a defect in the data the caller supplied,
		// never a transient database problem: retrying the same input cannot
		// succeed, so it must classify as permanent.
		return fmt.Errorf("%w: column %s must not be null", ErrInvalid, pgErr.ColumnName)
	case "23514", "22001", "22003", "22P02":
		return fmt.Errorf("%w: constraint %s", ErrInvalid, pgErr.ConstraintName)
	}
	return err
}

var _ Workspace = (*sqlcgen.Queries)(nil)

// Repository owns the pool. It is safe for concurrent use.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository wraps a pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Pool exposes the pool for River's driver.
func (r *Repository) Pool() *pgxpool.Pool { return r.pool }

// InTx runs fn in one read-committed transaction. The workspace fn receives
// is valid only inside fn. A returned error rolls everything back, and a
// panic rolls back and propagates.
func (r *Repository) InTx(ctx context.Context, fn func(ctx context.Context, ws *sqlcgen.Queries) error) (err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()
	if err = fn(ctx, sqlcgen.New(tx)); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// Read runs fn in one repeatable-read read-only transaction, so an assembled
// document is a consistent snapshot.
func (r *Repository) Read(ctx context.Context, fn func(ctx context.Context, rd Reader) error) (err error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; nothing to commit
	return fn(ctx, sqlcgen.New(tx))
}

// GetJob reads one ingest job row.
func (r *Repository) GetJob(ctx context.Context, id pgtype.UUID) (row sqlcgen.IngestJob, err error) {
	err = r.Read(ctx, func(ctx context.Context, rd Reader) error {
		var err error
		row, err = rd.GetIngestJob(ctx, id)
		return err
	})
	return row, Translate(err)
}

// Claim performs the conditional worker-start update.
func (r *Repository) Claim(ctx context.Context, id pgtype.UUID, from []JobState) (row sqlcgen.IngestJob, err error) {
	err = r.InTx(ctx, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		row, err = q.ClaimIngestJob(ctx, sqlcgen.ClaimIngestJobParams{JobID: id, States: from})
		return err
	})
	return row, Translate(err)
}

// Move performs one conditional state update.
func (r *Repository) Move(ctx context.Context, id pgtype.UUID, to JobState, from []JobState) (row sqlcgen.IngestJob, err error) {
	err = r.InTx(ctx, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		row, err = q.MoveIngestJob(ctx, sqlcgen.MoveIngestJobParams{JobID: id, State: to, FromStates: from})
		return err
	})
	return row, Translate(err)
}

// Fail records a permanent failure.
func (r *Repository) Fail(ctx context.Context, id pgtype.UUID, from []JobState, code, message string) (row sqlcgen.IngestJob, err error) {
	err = r.InTx(ctx, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		row, err = q.FailIngestJob(ctx, sqlcgen.FailIngestJobParams{
			JobID:        id,
			ErrorCode:    pgtype.Text{String: code, Valid: true},
			ErrorMessage: pgtype.Text{String: message, Valid: true},
			FromStates:   from,
		})
		return err
	})
	return row, Translate(err)
}

// pgUUID converts a Google UUID to the pgx wire type.
func pgUUID(id uuid.UUID) pgtype.UUID {
	var out pgtype.UUID
	copy(out.Bytes[:], id[:])
	out.Valid = true
	return out
}

func fromPGUUID(v pgtype.UUID) (uuid.UUID, error) {
	if !v.Valid {
		return uuid.Nil, fmt.Errorf("%w: empty uuid", ErrInvalid)
	}
	var out uuid.UUID
	copy(out[:], v.Bytes[:])
	return out, nil
}

// WorkspaceTx binds a pgx transaction to the Workspace the assembler uses.
func WorkspaceTx(tx pgx.Tx) *sqlcgen.Queries { return sqlcgen.New(tx) }

// AssetRecord implements storage.AssetLookup for the renderer resolver and the
// asset endpoints. Every variant column is copied; a NULL column stays empty so
// the resolver can tell "not present" from "present but unverified".
func (r *Repository) AssetRecord(ctx context.Context, assetID string) (rec storage.AssetRecord, err error) {
	err = r.Read(ctx, func(ctx context.Context, rd Reader) error {
		row, err := rd.GetAsset(ctx, assetID)
		if err != nil {
			return err
		}
		rec = AssetRecordFromRow(row)
		return nil
	})
	return rec, Translate(err)
}

// AssetRecordFromRow maps one assets row to the resolver's record.
func AssetRecordFromRow(row sqlcgen.Asset) storage.AssetRecord {
	return storage.AssetRecord{
		AssetID:         row.AssetID,
		MediaType:       row.MediaType,
		Status:          row.Status,
		StorageKey:      row.StorageKey,
		SHA256:          row.Sha256,
		RawStorageKey:   row.RawStorageKey.String,
		RawSHA256:       row.RawSha256.String,
		CleanStorageKey: row.CleanStorageKey.String,
		CleanSHA256:     row.CleanSha256.String,
		ActiveVariant:   storage.Variant(row.ActiveVariant),
		Revision:        row.Revision,
	}
}

// Ping reports whether PostgreSQL answers within ctx. /readyz uses it with a
// short deadline.
func (r *Repository) Ping(ctx context.Context) error {
	return Translate(r.pool.Ping(ctx))
}
