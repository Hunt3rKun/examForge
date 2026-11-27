package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store/sqlcgen"
)

// MaxAssets bounds the asset metadata rows one ingest may write.
const MaxAssets = 4096

// AssetURIScheme prefixes the resolver-neutral URI of an assembled asset.
// The URI names an asset; it is never a file path or an object key.
const AssetURIScheme = "examforge-asset://"

// VerifiedAsset is an asset object that was uploaded and hashed before the
// persistence transaction. Persist records the metadata; it never uploads.
//
// raw_cropped: StorageKey names the raw object, SHA256 verifies it, and the
// raw companion fields are empty. cleaned: StorageKey names the clean object
// (verified by SHA256) and RawStorageKey/RawSHA256 name and verify the
// distinct raw companion object.
type VerifiedAsset struct {
	AssetID       string
	StorageKey    string
	RawStorageKey string // empty when no raw companion was kept
	SHA256        string
	RawSHA256     string // digest of the raw companion; empty when none
}

// SourceRecord ties one persistence to its ingest job and its uploaded objects.
type SourceRecord struct {
	JobID        uuid.UUID
	SourcePDFKey string
	SourceSHA256 string
	Assets       []VerifiedAsset
}

// ExamRecord summarizes one persisted exam.
type ExamRecord struct {
	ExamID      uuid.UUID
	IngestJobID uuid.UUID
	Sections    int
	Items       int
	Questions   int
}

// Assembler maps Exam IR to the relational model and back.
type Assembler struct {
	validator *ir.Validator
}

// NewAssembler requires the compiled schemas: every Persist and Assemble
// runs the full schema and semantic validation.
func NewAssembler(validator *ir.Validator) (*Assembler, error) {
	if validator == nil {
		return nil, errors.New("store: assembler needs a validator")
	}
	return &Assembler{validator: validator}, nil
}

// PersistTx is the specification's transaction form: the caller owns the pgx
// transaction and commits or rolls it back.
func (a *Assembler) PersistTx(ctx context.Context, tx pgx.Tx, exam ir.ExamDocumentIR, source SourceRecord) (ExamRecord, error) {
	return a.Persist(ctx, sqlcgen.New(tx), exam, source)
}

type resolvedQuestion struct {
	id      pgtype.UUID
	content ir.QuestionItem
}

// Persist validates the exam, then writes asset metadata, questions, the exam
// and its sections/items, and completes the ingest job, all in the caller's
// transaction. A failing step leaves nothing behind once it rolls back.
//
// Asset rule: same asset_id and same SHA-256 is idempotent; a different
// digest or incompatible metadata is ErrConflict and nothing is overwritten.
// Question rule: a candidate with the same fingerprint is reused only when
// its stored content equals the incoming question apart from question_id,
// display_number and score. Otherwise a new entity is created; the
// projections of an existing entity are never overwritten.
func (a *Assembler) Persist(ctx context.Context, ws Workspace, exam ir.ExamDocumentIR, source SourceRecord) (rec ExamRecord, err error) {
	defer func() {
		if err != nil {
			err = Translate(err)
		}
	}()
	if err := a.validator.ValidateExamDTO(exam); err != nil {
		return ExamRecord{}, fmt.Errorf("%w: exam IR rejected: %w", ErrInvalid, err)
	}
	if source.JobID == uuid.Nil || !storage.ValidSHA256(source.SourceSHA256) || len(exam.Assets) > MaxAssets {
		return ExamRecord{}, fmt.Errorf("%w: source record is incomplete", ErrInvalid)
	}
	if exam.Metadata.Subject == "" {
		return ExamRecord{}, fmt.Errorf("%w: metadata.subject must not be empty", ErrInvalid)
	}

	// A replayed job refreshes its existing exam instead of adding another.
	existing, err := ws.GetExamByJob(ctx, pgUUID(source.JobID))
	var examID pgtype.UUID
	replay := false
	switch {
	case err == nil:
		var id uuid.UUID
		if id, err = fromPGUUID(existing.ExamID); err != nil {
			return ExamRecord{}, err
		}
		if existing.SourceFileSha256 != source.SourceSHA256 {
			return ExamRecord{}, fmt.Errorf("%w: job %s already produced an exam from a different source", ErrConflict, source.JobID)
		}
		examID, replay = pgUUID(id), true
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
	default:
		return ExamRecord{}, err
	}

	if err := a.persistAssets(ctx, ws, exam, source); err != nil {
		return ExamRecord{}, err
	}

	entities := map[pgtype.UUID]bool{}
	reused := map[string]resolvedQuestion{}
	metadataJSON, err := json.Marshal(exam.Metadata)
	if err != nil {
		return ExamRecord{}, fmt.Errorf("%w: metadata: %v", ErrInvalid, err)
	}
	plan := make([][]resolvedQuestion, len(exam.Sections))
	itemCount := 0
	for si, section := range exam.Sections {
		plan[si] = make([]resolvedQuestion, len(section.Questions))
		for qi, question := range section.Questions {
			entity, err := a.resolveQuestion(ctx, ws, exam, question, reused)
			if err != nil {
				return ExamRecord{}, fmt.Errorf("sections[%d].questions[%d]: %w", si, qi, err)
			}
			plan[si][qi] = entity
			entities[entity.id] = true
			itemCount++
		}
	}

	// Every path that adds an exam_items reference takes the question row
	// locks first, in sorted ID order, so a concurrent edit's reference count
	// cannot miss this attach and two writers cannot deadlock. Rows this
	// transaction just inserted are already locked; locking them again is a
	// no-op.
	if err := lockQuestionSet(ctx, ws, entities); err != nil {
		return ExamRecord{}, err
	}

	examParams := sqlcgen.UpdateExamParams{
		IrExamID:         exam.ExamID,
		SourceFileSha256: source.SourceSHA256,
		SourcePdfKey:     source.SourcePDFKey,
		SchemaVersion:    exam.SchemaVersion,
		MetadataJson:     metadataJSON,
	}
	if replay {
		examParams.ExamID = examID
		if _, err := ws.UpdateExam(ctx, examParams); err != nil {
			return ExamRecord{}, fmt.Errorf("update exam: %w", err)
		}
	} else {
		row, err := ws.InsertExam(ctx, sqlcgen.InsertExamParams{
			IngestJobID:      pgUUID(source.JobID),
			IrExamID:         exam.ExamID,
			SourceFileSha256: source.SourceSHA256,
			SourcePdfKey:     source.SourcePDFKey,
			SchemaVersion:    exam.SchemaVersion,
			MetadataJson:     metadataJSON,
		})
		if err != nil {
			return ExamRecord{}, fmt.Errorf("insert exam: %w", err)
		}
		examID = row.ExamID
	}
	if _, err := ws.DeleteSections(ctx, examID); err != nil {
		return ExamRecord{}, fmt.Errorf("replace sections: %w", err)
	}
	for si, section := range exam.Sections {
		sec, err := ws.InsertSection(ctx, sqlcgen.InsertSectionParams{
			ExamID:      examID,
			IrSectionID: section.SectionID,
			Ordinal:     int32(si),
			Title:       section.Title,
			Instruction: textOrNil(section.Instruction),
		})
		if err != nil {
			return ExamRecord{}, fmt.Errorf("insert section %q: %w", section.SectionID, err)
		}
		for qi, entity := range plan[si] {
			incoming := section.Questions[qi]
			if err := ws.InsertItem(ctx, sqlcgen.InsertItemParams{
				SectionID:             sec.SectionID,
				QuestionID:            entity.id,
				Ordinal:               int32(qi),
				DisplayNumberOverride: occurrenceText(entity.content.DisplayNumber, incoming.DisplayNumber),
				ScoreOverride:         occurrenceFloat(entity.content.Score, incoming.Score),
				AnswerSpaceMmOverride: occurrenceFloat(0, incoming.AnswerSpaceMM),
			}); err != nil {
				return ExamRecord{}, fmt.Errorf("insert item %d of section %q: %w", qi, section.SectionID, err)
			}
		}
	}

	// The job leaves the active state in the same transaction as its exam.
	if _, err := ws.CompleteIngestJob(ctx, sqlcgen.CompleteIngestJobParams{
		JobID:  pgUUID(source.JobID),
		ExamID: examID,
	}); err != nil {
		return ExamRecord{}, fmt.Errorf("complete ingest job: %w", err)
	}
	id, _ := fromPGUUID(examID)
	return ExamRecord{ExamID: id, IngestJobID: source.JobID, Sections: len(exam.Sections), Items: itemCount, Questions: len(entities)}, nil
}

// persistAssets verifies every IR asset against its uploaded object and
// writes or checks its metadata row.
func (a *Assembler) persistAssets(ctx context.Context, ws Workspace, exam ir.ExamDocumentIR, source SourceRecord) error {
	uploads := make(map[string]VerifiedAsset, len(source.Assets))
	for _, upload := range source.Assets {
		if _, dup := uploads[upload.AssetID]; dup {
			return fmt.Errorf("%w: asset %q is uploaded twice", ErrInvalid, upload.AssetID)
		}
		if !storage.ValidSHA256(upload.SHA256) || storage.ValidateKey(upload.StorageKey) != nil {
			return fmt.Errorf("%w: asset %q has an incomplete upload record", ErrInvalid, upload.AssetID)
		}
		if (upload.RawStorageKey == "") != (upload.RawSHA256 == "") {
			return fmt.Errorf("%w: asset %q raw companion key and digest must be present together", ErrInvalid, upload.AssetID)
		}
		if upload.RawStorageKey != "" &&
			(!storage.ValidSHA256(upload.RawSHA256) || storage.ValidateKey(upload.RawStorageKey) != nil) {
			return fmt.Errorf("%w: asset %q has an invalid raw companion record", ErrInvalid, upload.AssetID)
		}
		uploads[upload.AssetID] = upload
	}
	for id := range uploads {
		if _, ok := exam.Assets[id]; !ok {
			return fmt.Errorf("%w: asset %q was uploaded but is not part of the exam", ErrInvalid, id)
		}
	}
	ids := make([]string, 0, len(exam.Assets))
	for id := range exam.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		asset := exam.Assets[id]
		upload, ok := uploads[id]
		if !ok {
			return fmt.Errorf("%w: asset %q has no uploaded object", ErrInvalid, id)
		}
		rawKey, err := storage.RawAssetKey(id, asset.MediaType)
		if err != nil {
			return fmt.Errorf("%w: asset %q: %v", ErrInvalid, id, err)
		}
		wantStorage := rawKey
		// Every new row names both variants explicitly: the selector and the
		// per-variant key/digest columns are never left to the column default,
		// because a raw-only asset and a cleaned asset differ in which variant
		// is active and which columns are populated.
		incoming := sqlcgen.InsertAssetParams{
			AssetID:          id,
			MediaType:        asset.MediaType,
			StorageKey:       upload.StorageKey,
			Status:           asset.Status,
			AspectRatio:      positiveOrNil(asset.AspectRatio),
			SuggestedWidthMm: positiveOrNil(asset.SuggestedWidthMM),
			Caption:          textOrNil(asset.Caption),
			Alt:              textOrNil(asset.Alt),
			Sha256:           upload.SHA256,
		}
		if asset.Status == "raw_cropped" {
			// A raw-only asset stores one object: no companion, no second digest.
			if upload.RawStorageKey != "" || upload.RawSHA256 != "" {
				return fmt.Errorf("%w: asset %q is raw_cropped but carries a raw companion record", ErrInvalid, id)
			}
			// The main object is the raw variant; there is no clean variant.
			incoming.RawStorageKey = textOrNil(upload.StorageKey)
			incoming.RawSha256 = textOrNil(upload.SHA256)
			incoming.ActiveVariant = string(storage.VariantRaw)
		} else {
			if wantStorage, err = storage.CleanAssetKey(id, asset.MediaType); err != nil {
				return fmt.Errorf("%w: asset %q: %v", ErrInvalid, id, err)
			}
			if upload.RawStorageKey != rawKey || !storage.ValidSHA256(upload.RawSHA256) {
				return fmt.Errorf("%w: asset %q needs a raw companion key and a valid raw digest", ErrInvalid, id)
			}
			// The main object is the clean variant; the companion is raw.
			incoming.CleanStorageKey = textOrNil(upload.StorageKey)
			incoming.CleanSha256 = textOrNil(upload.SHA256)
			incoming.RawStorageKey = textOrNil(upload.RawStorageKey)
			incoming.RawSha256 = textOrNil(upload.RawSHA256)
			incoming.ActiveVariant = string(storage.VariantClean)
		}
		if upload.StorageKey != wantStorage {
			return fmt.Errorf("%w: asset %q keys do not follow the deterministic key rule", ErrInvalid, id)
		}
		bbox, err := json.Marshal(asset.SourceBBox)
		if err != nil {
			return fmt.Errorf("%w: asset %q source_bbox: %v", ErrInvalid, id, err)
		}
		incoming.SourceBbox = bbox
		row, err := ws.GetAsset(ctx, id)
		created := false
		switch {
		case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
			// Insert with ON CONFLICT DO NOTHING. A concurrent transaction may
			// have created the row between the read and this insert; that
			// returns no row, and the re-read below applies the same rules.
			row, err = ws.InsertAsset(ctx, incoming)
			switch {
			case errors.Is(err, ErrNotFound):
				// Lost the race: verify the winner's row exactly as if it had
				// been there all along.
				row, err = ws.GetAsset(ctx, id)
			case err == nil:
				created = true
			}
			if err != nil {
				return fmt.Errorf("insert asset %q: %w", id, err)
			}
		case err != nil:
			return fmt.Errorf("read asset %q: %w", id, err)
		}
		if row.Sha256 != upload.SHA256 {
			// Same ID, different bytes: fail and keep the registered object.
			return fmt.Errorf("%w: asset %q is registered with SHA-256 %s but %s was uploaded; nothing was overwritten",
				ErrConflict, id, row.Sha256, upload.SHA256)
		}
		// The raw digest is part of the identity when both sides declare one.
		// A legacy row (raw_sha256 IS NULL) predates the digest contract: its
		// raw companion cannot be verified, so the stored row is kept as is
		// and only the main object remains authoritative.
		if row.RawSha256.Valid && incoming.RawSha256.Valid && row.RawSha256.String != incoming.RawSha256.String {
			return fmt.Errorf("%w: asset %q raw companion is registered with a different SHA-256; nothing was overwritten", ErrConflict, id)
		}
		if !sameAssetIdentity(row, incoming) {
			// Same bytes but different identity metadata is a conflict, never a
			// silent update of what the asset is.
			return fmt.Errorf("%w: asset %q exists with different media type, status, geometry or keys", ErrConflict, id)
		}
		if created {
			// The insert already wrote this call's caption and alt.
			continue
		}
		// caption and alt are display text: a non-empty incoming value is the
		// one allowed update; an empty one keeps the registered text.
		if _, err := ws.UpdateAssetDisplay(ctx, sqlcgen.UpdateAssetDisplayParams{
			AssetID: id, Caption: asset.Caption, Alt: asset.Alt,
		}); err != nil {
			return fmt.Errorf("update asset %q display text: %w", id, err)
		}
	}
	return nil
}

// sameAssetIdentity reports whether an existing row describes the same asset
// as the incoming record: the identity fields, not the display text, and not
// the mutable selector (active_variant) or revision.
func sameAssetIdentity(row sqlcgen.Asset, incoming sqlcgen.InsertAssetParams) bool {
	return row.MediaType == incoming.MediaType && row.Status == incoming.Status &&
		row.StorageKey == incoming.StorageKey && sameText(row.RawStorageKey, incoming.RawStorageKey) &&
		cleanKey(row) == textValue(incoming.CleanStorageKey) &&
		sameFloat(row.AspectRatio, incoming.AspectRatio) &&
		sameFloat(row.SuggestedWidthMm, incoming.SuggestedWidthMm) &&
		sameJSON(row.SourceBbox, incoming.SourceBbox)
}

// cleanKey returns the row's clean object key, applying the pre-migration
// fallback: a cleaned row written before migration 0007 has no clean column,
// and its main key is the clean object. A raw_cropped row has no clean object.
func cleanKey(row sqlcgen.Asset) string {
	if row.CleanStorageKey.Valid && row.CleanStorageKey.String != "" {
		return row.CleanStorageKey.String
	}
	if row.Status == "cleaned" {
		return row.StorageKey
	}
	return ""
}

// resolveQuestion returns the entity for one occurrence, creating it when no
// equal-content candidate exists. The cache holds entities selected or created
// in this transaction, so repeated content inside one exam shares an entity.
//
// The candidate lookup runs under the question row locks (FOR UPDATE): two
// concurrent ingests of the same content must not both conclude no candidate
// exists and write two equal entities for one occurrence set. The rows it
// returns are already locked by this transaction, so the reference the caller
// inserts is covered by the lock protocol.
func (a *Assembler) resolveQuestion(ctx context.Context, ws Workspace, exam ir.ExamDocumentIR, q ir.QuestionItem, cache map[string]resolvedQuestion) (resolvedQuestion, error) {
	fp, err := QuestionFingerprint(q)
	if err != nil {
		return resolvedQuestion{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if cached, ok := cache[fp]; ok && sameQuestionContent(cached.content, q) {
		return cached, nil
	}
	candidates, err := ws.ListQuestionsByFingerprintForUpdate(ctx, sqlcgen.ListQuestionsByFingerprintForUpdateParams{
		ContentFingerprint: fp, FingerprintVersion: FingerprintVersion,
	})
	if err != nil {
		return resolvedQuestion{}, fmt.Errorf("lookup fingerprint: %w", err)
	}
	for _, c := range candidates {
		content, err := decodeQuestion(c.ContentJson, c.QuestionID)
		if err != nil {
			// A stored row this build cannot read is reported, never merged.
			return resolvedQuestion{}, err
		}
		if sameQuestionContent(content, q) {
			r := resolvedQuestion{id: c.QuestionID, content: content}
			cache[fp] = r
			return r, nil
		}
	}
	contentJSON, err := json.Marshal(entityContent(q))
	if err != nil {
		return resolvedQuestion{}, fmt.Errorf("%w: question content: %v", ErrInvalid, err)
	}
	points, difficulty, err := projectMetadata(q.Metadata)
	if err != nil {
		return resolvedQuestion{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	row, err := ws.InsertQuestion(ctx, sqlcgen.InsertQuestionParams{
		Subject:            exam.Metadata.Subject,
		GradeLevel:         textOrNil(exam.Metadata.Grade),
		Kind:               q.Kind,
		QuestionType:       textOrNil(q.QuestionType),
		KnowledgePoints:    points,
		Difficulty:         difficulty,
		ContentFingerprint: fp,
		FingerprintVersion: FingerprintVersion,
		ContentJson:        contentJSON,
	})
	if err != nil {
		return resolvedQuestion{}, fmt.Errorf("insert question: %w", err)
	}
	r := resolvedQuestion{id: row.QuestionID, content: q}
	cache[fp] = r
	return r, nil
}

// lockQuestionSet takes the sorted question row locks for every entity one
// write is about to reference. The sort happens in SQL (ORDER BY question_id),
// which is the order every writer uses, so the protocol cannot deadlock.
func lockQuestionSet(ctx context.Context, ws Workspace, ids map[pgtype.UUID]bool) error {
	if len(ids) == 0 {
		return nil
	}
	list := make([]pgtype.UUID, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	if _, err := ws.LockQuestions(ctx, list); err != nil {
		return fmt.Errorf("lock questions: %w", err)
	}
	return nil
}

// entityContent is what a question entity stores: the question's semantics.
// The top-level answer_space_mm is an occurrence value owned by exam_items
// since fingerprint version 2, so it never enters content_json. A composite's
// children keep theirs: they have no exam_items row of their own and the value
// is part of the composite's layout.
func entityContent(q ir.QuestionItem) ir.QuestionItem {
	q.AnswerSpaceMM = 0
	return q
}

// sameQuestionContent reports equality of everything except question_id,
// display_number, score and answer_space_mm, which are per-occurrence values
// owned by exam_items since fingerprint version 2.
func sameQuestionContent(a, b ir.QuestionItem) bool {
	a.QuestionID, a.DisplayNumber, a.Score, a.AnswerSpaceMM = "", "", 0, 0
	b.QuestionID, b.DisplayNumber, b.Score, b.AnswerSpaceMM = "", "", 0, 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func projectMetadata(meta map[string]any) ([]string, pgtype.Float8, error) {
	// A non-nil empty slice, not nil: pgx encodes a nil []string as SQL NULL,
	// which violates questions.knowledge_points NOT NULL DEFAULT '{}'. The
	// default only applies when the column is omitted from the INSERT.
	points := []string{}
	if raw, ok := meta["knowledge_points"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, pgtype.Float8{}, errors.New("metadata.knowledge_points is not an array")
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, pgtype.Float8{}, errors.New("metadata.knowledge_points contains a non-string")
			}
			points = append(points, s)
		}
	}
	var difficulty pgtype.Float8
	if raw, ok := meta["difficulty"]; ok {
		f, ok := raw.(float64)
		if !ok {
			return nil, pgtype.Float8{}, errors.New("metadata.difficulty is not a number")
		}
		difficulty = pgtype.Float8{Float64: f, Valid: true}
	}
	return points, difficulty, nil
}

// Assemble rebuilds a complete ExamDocumentIR from one exam: metadata, the
// referenced assets, sections in ordinal order and each item's full content
// with its display-number and score overrides. Every question_id is the
// question entity UUID. The result is validated again; a corrupt or
// incomplete row set yields an error rather than a partial document.
func (a *Assembler) Assemble(ctx context.Context, rd Reader, examID uuid.UUID) (out ir.ExamDocumentIR, err error) {
	defer func() {
		if err != nil {
			err = Translate(err)
		}
	}()
	row, err := rd.GetExam(ctx, pgUUID(examID))
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ir.ExamDocumentIR{}, fmt.Errorf("%w: exam %s", ErrNotFound, examID)
	case err != nil:
		return ir.ExamDocumentIR{}, fmt.Errorf("read exam: %w", err)
	}
	var metadata ir.ExamMetadata
	if err := decodeStrict(row.MetadataJson, &metadata, "exams.metadata_json"); err != nil {
		return ir.ExamDocumentIR{}, err
	}
	sections, err := rd.ListSections(ctx, row.ExamID)
	if err != nil {
		return ir.ExamDocumentIR{}, fmt.Errorf("read sections: %w", err)
	}
	if len(sections) == 0 {
		return ir.ExamDocumentIR{}, fmt.Errorf("%w: exam %s has no sections", ErrInvalid, examID)
	}
	var (
		itemsBySection = make([][]sqlcgen.ExamItem, len(sections))
		questionIDs    []pgtype.UUID
	)
	for si, sec := range sections {
		if sec.ExamID != row.ExamID || int(sec.Ordinal) != si {
			return ir.ExamDocumentIR{}, fmt.Errorf("%w: section ordinals are not contiguous from 0 at position %d", ErrInvalid, si)
		}
		items, err := rd.ListItems(ctx, sec.SectionID)
		if err != nil {
			return ir.ExamDocumentIR{}, fmt.Errorf("read items of section %q: %w", sec.IrSectionID, err)
		}
		if len(items) == 0 {
			return ir.ExamDocumentIR{}, fmt.Errorf("%w: section %q has no items", ErrInvalid, sec.IrSectionID)
		}
		for qi, item := range items {
			if item.SectionID != sec.SectionID || int(item.Ordinal) != qi {
				return ir.ExamDocumentIR{}, fmt.Errorf("%w: item ordinals of section %q are not contiguous from 0", ErrInvalid, sec.IrSectionID)
			}
			questionIDs = append(questionIDs, item.QuestionID)
		}
		itemsBySection[si] = items
	}
	questions, err := a.loadQuestions(ctx, rd, questionIDs)
	if err != nil {
		return ir.ExamDocumentIR{}, err
	}
	assets, err := a.loadAssets(ctx, rd, questions, itemsBySection)
	if err != nil {
		return ir.ExamDocumentIR{}, err
	}

	out = ir.ExamDocumentIR{
		SchemaVersion: row.SchemaVersion,
		ExamID:        row.IrExamID,
		Metadata:      metadata,
		Assets:        assets,
		Sections:      make([]ir.Section, 0, len(sections)),
	}
	for si, sec := range sections {
		section := ir.Section{SectionID: sec.IrSectionID, Title: sec.Title, Instruction: sec.Instruction.String}
		for _, item := range itemsBySection[si] {
			content := questions[item.QuestionID]
			// The occurrence values come from exam_items only. answer_space_mm
			// is reset first: a legacy entity row still carries the value inside
			// content_json, and that stored copy must never override the item.
			content.AnswerSpaceMM = 0
			if item.DisplayNumberOverride.Valid {
				content.DisplayNumber = item.DisplayNumberOverride.String
			}
			if item.ScoreOverride.Valid {
				content.Score = item.ScoreOverride.Float64
			}
			if item.AnswerSpaceMmOverride.Valid {
				content.AnswerSpaceMM = item.AnswerSpaceMmOverride.Float64
			}
			section.Questions = append(section.Questions, content)
		}
		out.Sections = append(out.Sections, section)
	}
	if err := a.validator.ValidateExamDTO(out); err != nil {
		return ir.ExamDocumentIR{}, fmt.Errorf("%w: assembled exam fails validation: %w", ErrInvalid, err)
	}
	return out, nil
}

// loadQuestions reads every referenced question in one batch. The returned
// content carries the entity UUID as question_id.
func (a *Assembler) loadQuestions(ctx context.Context, rd Reader, ids []pgtype.UUID) (map[pgtype.UUID]ir.QuestionItem, error) {
	rows, err := rd.ListQuestions(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read questions: %w", err)
	}
	unique := map[pgtype.UUID]bool{}
	for _, id := range ids {
		unique[id] = true
	}
	if len(rows) != len(unique) {
		return nil, fmt.Errorf("%w: %d items reference %d distinct questions but %d were found",
			ErrInvalid, len(ids), len(unique), len(rows))
	}
	out := make(map[pgtype.UUID]ir.QuestionItem, len(rows))
	for _, row := range rows {
		content, err := decodeQuestion(row.ContentJson, row.QuestionID)
		if err != nil {
			return nil, err
		}
		// Projections must agree with the stored content.
		if row.Kind != content.Kind || row.QuestionType.String != content.QuestionType {
			return nil, fmt.Errorf("%w: question %s projections disagree with content_json", ErrInvalid, row.QuestionID)
		}
		entityID, err := fromPGUUID(row.QuestionID)
		if err != nil {
			return nil, err
		}
		content.QuestionID = entityID.String()
		out[row.QuestionID] = content
	}
	return out, nil
}

// loadAssets returns every asset referenced by the questions. An unreferenced
// asset is not part of the exam and is not returned.
func (a *Assembler) loadAssets(ctx context.Context, rd Reader, questions map[pgtype.UUID]ir.QuestionItem, itemsBySection [][]sqlcgen.ExamItem) (map[string]ir.Asset, error) {
	referenced := map[string]bool{}
	for _, items := range itemsBySection {
		for _, item := range items {
			collectAssetRefs(questions[item.QuestionID], referenced)
		}
	}
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return map[string]ir.Asset{}, nil
	}
	rows, err := rd.ListAssets(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read assets: %w", err)
	}
	out := make(map[string]ir.Asset, len(rows))
	for _, row := range rows {
		asset, err := toIRAsset(row)
		if err != nil {
			return nil, err
		}
		out[row.AssetID] = asset
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("%w: asset %q referenced by the exam is not registered", ErrNotFound, id)
		}
	}
	return out, nil
}

// collectAssetRefs gathers every image_ref asset ID at any nesting depth.
func collectAssetRefs(q ir.QuestionItem, into map[string]bool) {
	blocks := func(list []ir.ContentBlock) {
		for _, block := range list {
			if block.Type == "image_ref" {
				into[block.AssetID] = true
			}
		}
	}
	blocks(q.Stem)
	for _, option := range q.Options {
		blocks(option.Content)
	}
	if q.Answer != nil {
		blocks(q.Answer.Detail)
	}
	blocks(q.Analysis)
	for _, child := range q.Children {
		collectAssetRefs(child, into)
	}
}

func toIRAsset(row sqlcgen.Asset) (ir.Asset, error) {
	asset := ir.Asset{
		AssetID:   row.AssetID,
		MediaType: row.MediaType,
		URI:       AssetURIScheme + row.AssetID,
		Status:    row.Status,
		Caption:   row.Caption.String,
		Alt:       row.Alt.String,
	}
	if row.AspectRatio.Valid {
		asset.AspectRatio = row.AspectRatio.Float64
	}
	if row.SuggestedWidthMm.Valid {
		asset.SuggestedWidthMM = row.SuggestedWidthMm.Float64
	}
	if len(row.SourceBbox) > 0 && string(row.SourceBbox) != "null" {
		if err := decodeStrict(row.SourceBbox, &asset.SourceBBox, "assets.source_bbox for "+row.AssetID); err != nil {
			return ir.Asset{}, err
		}
	}
	if row.RawStorageKey.Valid && row.RawStorageKey.String != "" && row.RawStorageKey.String != row.StorageKey {
		asset.RawURI = AssetURIScheme + row.AssetID + "/raw"
	}
	return asset, nil
}

// decodeQuestion strictly decodes one stored QuestionItem. An unknown field
// or a wrong type is an error: this build must not drop content it cannot read.
func decodeQuestion(data []byte, id pgtype.UUID) (ir.QuestionItem, error) {
	var q ir.QuestionItem
	if err := decodeStrict(data, &q, "questions.content_json for "+uuidString(id)); err != nil {
		return ir.QuestionItem{}, err
	}
	return q, nil
}

func decodeStrict(data []byte, v any, what string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, what, err)
	}
	return nil
}

func uuidString(id pgtype.UUID) string {
	out, err := fromPGUUID(id)
	if err != nil {
		return "<invalid uuid>"
	}
	return out.String()
}

// occurrenceText keeps an override only when the occurrence differs from the
// entity's stored display number.
func occurrenceText(stored, incoming string) pgtype.Text {
	if incoming == stored {
		return pgtype.Text{}
	}
	return pgtype.Text{String: incoming, Valid: true}
}

func occurrenceFloat(stored, incoming float64) pgtype.Float8 {
	if incoming == stored {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: incoming, Valid: true}
}

func textOrNil(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func textValue(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func positiveOrNil(f float64) pgtype.Float8 { return pgtype.Float8{Float64: f, Valid: f > 0} }

func sameFloat(a, b pgtype.Float8) bool {
	return a.Valid == b.Valid && (!a.Valid || a.Float64 == b.Float64)
}

func sameText(a, b pgtype.Text) bool { return a.Valid == b.Valid && (!a.Valid || a.String == b.String) }

// sameJSON compares two JSON documents by value (key order and whitespace do
// not matter, as with PostgreSQL jsonb).
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}
