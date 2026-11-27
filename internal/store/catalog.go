package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/store/sqlcgen"
)

// JobSummary is the transport-neutral view of one ingest job. It carries no
// source key, no River payload and no unredacted provider output: the API DTO
// maps exactly these fields.
type JobSummary struct {
	JobID        uuid.UUID
	State        string
	AttemptCount int32
	ExamID       *uuid.UUID
	ErrorCode    string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// JobSummaryFromRow maps one ingest_jobs row.
func JobSummaryFromRow(row sqlcgen.IngestJob) JobSummary {
	out := JobSummary{
		JobID:        uuid.UUID(row.JobID.Bytes),
		State:        row.State,
		AttemptCount: row.AttemptCount,
		ErrorCode:    row.ErrorCode.String,
		ErrorMessage: row.ErrorMessage.String,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
	if row.ExamID.Valid {
		id := uuid.UUID(row.ExamID.Bytes)
		out.ExamID = &id
	}
	if row.StartedAt.Valid {
		t := row.StartedAt.Time
		out.StartedAt = &t
	}
	if row.FinishedAt.Valid {
		t := row.FinishedAt.Time
		out.FinishedAt = &t
	}
	return out
}

// ExamSummary is the list-view projection of one exam.
type ExamSummary struct {
	ExamID        uuid.UUID
	IrExamID      string
	SchemaVersion string
	SourceSHA256  string
	Metadata      ir.ExamMetadata
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Sections      int
	Items         int
	Questions     int
	PendingReview int
}

// ExamSnapshot is one consistent view of an exam: the assembled document, the
// occurrence rows that produced it, the asset rows it references and the
// cross-exam reference counts of its question entities. Everything in it comes
// from the same read transaction, so an item and its revision cannot disagree.
type ExamSnapshot struct {
	Exam       sqlcgen.Exam
	Sections   []sqlcgen.ExamSection
	Items      []sqlcgen.ExamItem
	Assets     []sqlcgen.Asset
	References map[string]int64 // question entity UUID -> occurrences across all exams
	Document   ir.ExamDocumentIR
}

// SummarizeExam counts the exam's sections, items and distinct questions and
// how many of those questions still need a review decision. The counts come
// from the rows, not from a JSON scan of the assembled document.
func SummarizeExam(ctx context.Context, rd Reader, examID pgtype.UUID) (ExamSummary, error) {
	row, err := rd.GetExam(ctx, examID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ExamSummary{}, fmt.Errorf("%w: exam %s", ErrNotFound, uuidString(examID))
	case err != nil:
		return ExamSummary{}, fmt.Errorf("read exam: %w", err)
	}
	var metadata ir.ExamMetadata
	if err := decodeStrict(row.MetadataJson, &metadata, "exams.metadata_json"); err != nil {
		return ExamSummary{}, err
	}
	sections, err := rd.ListSections(ctx, examID)
	if err != nil {
		return ExamSummary{}, fmt.Errorf("read sections: %w", err)
	}
	items, err := rd.ListExamItems(ctx, examID)
	if err != nil {
		return ExamSummary{}, fmt.Errorf("read items: %w", err)
	}
	ids := make([]pgtype.UUID, 0, len(items))
	seen := map[pgtype.UUID]bool{}
	for _, item := range items {
		if !seen[item.QuestionID] {
			seen[item.QuestionID] = true
			ids = append(ids, item.QuestionID)
		}
	}
	questions, err := rd.ListQuestions(ctx, ids)
	if err != nil {
		return ExamSummary{}, fmt.Errorf("read questions: %w", err)
	}
	pending := 0
	for _, q := range questions {
		content, err := decodeQuestion(q.ContentJson, q.QuestionID)
		if err != nil {
			return ExamSummary{}, err
		}
		if NeedsReview(reviewStatusOf(content)) {
			pending++
		}
	}
	return ExamSummary{
		ExamID:        uuid.UUID(row.ExamID.Bytes),
		IrExamID:      row.IrExamID,
		SchemaVersion: row.SchemaVersion,
		SourceSHA256:  row.SourceFileSha256,
		Metadata:      metadata,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
		Sections:      len(sections),
		Items:         len(items),
		Questions:     len(ids),
		PendingReview: pending,
	}, nil
}

// LoadExamSnapshot assembles the exam and collects the occurrence, asset and
// reference metadata in the same read. The caller supplies the Reader of one
// consistent transaction.
func LoadExamSnapshot(ctx context.Context, a *Assembler, rd Reader, examID uuid.UUID) (ExamSnapshot, error) {
	if a == nil {
		return ExamSnapshot{}, errors.New("store: snapshot needs an assembler")
	}
	row, err := rd.GetExam(ctx, pgUUID(examID))
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ExamSnapshot{}, fmt.Errorf("%w: exam %s", ErrNotFound, examID)
	case err != nil:
		return ExamSnapshot{}, fmt.Errorf("read exam: %w", err)
	}
	document, err := a.Assemble(ctx, rd, examID)
	if err != nil {
		return ExamSnapshot{}, err
	}
	sections, err := rd.ListSections(ctx, row.ExamID)
	if err != nil {
		return ExamSnapshot{}, fmt.Errorf("read sections: %w", err)
	}
	items, err := rd.ListExamItems(ctx, row.ExamID)
	if err != nil {
		return ExamSnapshot{}, fmt.Errorf("read items: %w", err)
	}
	ids := make([]pgtype.UUID, 0, len(items))
	seen := map[pgtype.UUID]bool{}
	for _, item := range items {
		if !seen[item.QuestionID] {
			seen[item.QuestionID] = true
			ids = append(ids, item.QuestionID)
		}
	}
	references := map[string]int64{}
	if len(ids) > 0 {
		counts, err := rd.ListQuestionReferenceCounts(ctx, ids)
		if err != nil {
			return ExamSnapshot{}, fmt.Errorf("read reference counts: %w", err)
		}
		for _, count := range counts {
			references[uuidString(count.QuestionID)] = count.References
		}
	}
	assets, err := snapshotAssets(ctx, rd, document)
	if err != nil {
		return ExamSnapshot{}, err
	}
	return ExamSnapshot{
		Exam:       row,
		Sections:   sections,
		Items:      items,
		Assets:     assets,
		References: references,
		Document:   document,
	}, nil
}

// snapshotAssets reads the registry rows for every asset the document
// references, sorted by asset_id.
func snapshotAssets(ctx context.Context, rd Reader, document ir.ExamDocumentIR) ([]sqlcgen.Asset, error) {
	referenced := map[string]bool{}
	for _, section := range document.Sections {
		for _, question := range section.Questions {
			collectAssetRefs(question, referenced)
		}
	}
	if len(referenced) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows, err := rd.ListAssets(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read assets: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].AssetID < rows[j].AssetID })
	return rows, nil
}

// reviewStatusOf reads provenance.review_status as a string. A non-string or
// absent value means "never reviewed".
func reviewStatusOf(q ir.QuestionItem) string {
	if q.Provenance == nil {
		return ""
	}
	status, _ := q.Provenance["review_status"].(string)
	return status
}
