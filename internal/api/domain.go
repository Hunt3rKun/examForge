package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
)

type storeDomain struct {
	repo      *store.Repository
	assembler *store.Assembler
}

// NewStoreDomain adapts repository and assembler operations to the HTTP domain seam.
func NewStoreDomain(repo *store.Repository, assembler *store.Assembler, _ storage.ObjectStore, _ *ir.Validator) Domain {
	return &storeDomain{repo: repo, assembler: assembler}
}

type pageCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func encodeCursor(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(raw string) (pageCursor, error) {
	if raw == "" {
		return pageCursor{}, nil
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return pageCursor{}, store.ErrInvalid
	}
	var c pageCursor
	if json.Unmarshal(b, &c) != nil || c.At.IsZero() {
		return pageCursor{}, store.ErrInvalid
	}
	if _, e = uuid.Parse(c.ID); e != nil {
		return pageCursor{}, store.ErrInvalid
	}
	return c, nil
}
func cursorDB(c pageCursor) (pgtype.Timestamptz, pgtype.UUID, error) {
	if c.At.IsZero() {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	id, e := uuid.Parse(c.ID)
	if e != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, store.ErrInvalid
	}
	var u pgtype.UUID
	copy(u.Bytes[:], id[:])
	u.Valid = true
	return pgtype.Timestamptz{Time: c.At, Valid: true}, u, nil
}
func (d *storeDomain) ListJobs(ctx context.Context, limit int, cursor, state string) (out JobPage, err error) {
	c, e := decodeCursor(cursor)
	if e != nil {
		return out, e
	}
	at, id, e := cursorDB(c)
	if e != nil {
		return out, e
	}
	st := pgtype.Text{}
	if state != "" {
		st = pgtype.Text{String: state, Valid: true}
	}
	err = d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		rows, e := rd.ListIngestJobsPage(ctx, sqlcgen.ListIngestJobsPageParams{State: st, CursorCreatedAt: at, CursorJobID: id, PageLimit: int32(limit + 1)})
		if e != nil {
			return e
		}
		if len(rows) > limit {
			last := rows[limit-1]
			cursor := encodeCursor(pageCursor{At: last.CreatedAt.Time, ID: uuid.UUID(last.JobID.Bytes).String()})
			out.NextCursor = &cursor
			rows = rows[:limit]
		}
		out.Items = make([]JobDTO, 0, len(rows))
		for _, v := range rows {
			out.Items = append(out.Items, jobDTO(store.JobSummaryFromRow(v)))
		}
		return nil
	})
	return out, store.Translate(err)
}
func (d *storeDomain) GetJob(ctx context.Context, id uuid.UUID) (store.JobSummary, error) {
	row, e := d.repo.GetJob(ctx, uuidAsPG(id))
	if e != nil {
		return store.JobSummary{}, e
	}
	return store.JobSummaryFromRow(row), nil
}
func (d *storeDomain) ListExams(ctx context.Context, limit int, cursor string) (out ExamPage, err error) {
	c, e := decodeCursor(cursor)
	if e != nil {
		return out, e
	}
	at, id, e := cursorDB(c)
	if e != nil {
		return out, e
	}
	err = d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		rows, e := rd.ListExamsPage(ctx, sqlcgen.ListExamsPageParams{CursorCreatedAt: at, CursorExamID: id, PageLimit: int32(limit + 1)})
		if e != nil {
			return e
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		out.Items = make([]ExamListItem, 0, len(rows))
		for _, row := range rows {
			s, e := store.SummarizeExam(ctx, rd, uuidAsPG(uuid.UUID(row.ExamID.Bytes)))
			if e != nil {
				return e
			}
			out.Items = append(out.Items, ExamListItem{ExamID: s.ExamID, Title: s.Metadata.Title, Subject: s.Metadata.Subject, CreatedAt: s.CreatedAt, PendingReview: s.PendingReview, SectionCount: s.Sections, ItemCount: s.Items})
		}
		if more {
			last := rows[len(rows)-1]
			cursor := encodeCursor(pageCursor{At: last.CreatedAt.Time, ID: uuid.UUID(last.ExamID.Bytes).String()})
			out.NextCursor = &cursor
		}
		return nil
	})
	return out, store.Translate(err)
}
func uuidAsPG(id uuid.UUID) pgtype.UUID {
	var x pgtype.UUID
	copy(x.Bytes[:], id[:])
	x.Valid = true
	return x
}
func (d *storeDomain) GetExam(ctx context.Context, id uuid.UUID) (out ExamDetail, err error) {
	err = d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		snap, e := store.LoadExamSnapshot(ctx, d.assembler, rd, id)
		if e != nil {
			return e
		}
		exam := snap.Exam
		out.IR = snap.Document
		out.Metadata = ExamMetadata{ExamID: id, CreatedAt: exam.CreatedAt.Time, UpdatedAt: exam.UpdatedAt.Time, SourceSHA256: exam.SourceFileSha256}
		out.Items = make([]ItemMetadata, 0, len(snap.Items))
		sectionByID := map[pgtype.UUID]sqlcgen.ExamSection{}
		for _, s := range snap.Sections {
			sectionByID[s.SectionID] = s
		}
		for _, item := range snap.Items {
			section, ok := sectionByID[item.SectionID]
			if !ok {
				return fmt.Errorf("%w: section missing", store.ErrInvalid)
			}
			qid := uuid.UUID(item.QuestionID.Bytes)
			iid := uuid.UUID(item.ItemID.Bytes)
			rev := item.Revision
			out.Items = append(out.Items, ItemMetadata{SectionID: section.IrSectionID, Ordinal: int(item.Ordinal), ItemID: iid, QuestionID: qid.String(), Revision: rev, ETag: itemETag(iid, rev), Shared: snap.References[qid.String()] > 1})
		}
		out.Assets = make([]AssetMetadata, 0, len(snap.Assets))
		for _, row := range snap.Assets {
			out.Assets = append(out.Assets, assetDTO(store.AssetRecordFromRow(row)))
		}
		return nil
	})
	return out, store.Translate(err)
}
func (d *storeDomain) WithExam(ctx context.Context, id uuid.UUID, fn func(context.Context, ir.ExamDocumentIR) error) error {
	return store.Translate(d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		snap, e := store.LoadExamSnapshot(ctx, d.assembler, rd, id)
		if e != nil {
			return e
		}
		return fn(ctx, snap.Document)
	}))
}
func (d *storeDomain) GetSource(ctx context.Context, id uuid.UUID) (out SourceObject, err error) {
	err = d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		row, e := rd.GetExam(ctx, uuidAsPG(id))
		if e != nil {
			return e
		}
		out = SourceObject{Key: row.SourcePdfKey, SHA256: row.SourceFileSha256}
		return nil
	})
	return out, store.Translate(err)
}
func (d *storeDomain) PatchItem(ctx context.Context, exam, item uuid.UUID, revision int64, p store.ItemPatch) (out store.ItemEditResult, err error) {
	err = d.repo.InTx(ctx, func(ctx context.Context, ws *sqlcgen.Queries) error {
		var e error
		out, e = d.assembler.EditItem(ctx, ws, exam, item, revision, p)
		return e
	})
	return out, store.Translate(err)
}
func (d *storeDomain) GetAsset(ctx context.Context, id string) (out storage.AssetRecord, err error) {
	err = d.repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		row, e := rd.GetAsset(ctx, id)
		if e != nil {
			return e
		}
		out = store.AssetRecordFromRow(row)
		return nil
	})
	return out, store.Translate(err)
}
func (d *storeDomain) SetActiveVariant(ctx context.Context, id string, v storage.Variant, revision int64) (out storage.AssetRecord, err error) {
	err = d.repo.InTx(ctx, func(ctx context.Context, ws *sqlcgen.Queries) error {
		var e error
		out, e = store.SetActiveVariant(ctx, ws, id, v, revision)
		return e
	})
	return out, store.Translate(err)
}

var _ Domain = (*storeDomain)(nil)
