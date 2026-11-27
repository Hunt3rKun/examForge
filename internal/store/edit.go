package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store/sqlcgen"
)

// RevisionConflictError reports a conditional write whose If-Match token no
// longer matches the row. Current is the revision the row has now, so the
// transport layer can answer 409 with a fresh ETag instead of silently
// overwriting a concurrent edit. It unwraps to ErrConflict.
type RevisionConflictError struct {
	Resource string
	Current  int64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%v: %s revision is %d", ErrConflict, e.Resource, e.Current)
}

// Unwrap makes errors.Is(err, ErrConflict) true.
func (e *RevisionConflictError) Unwrap() error { return ErrConflict }

// ReviewStatuses is the review_status enum. It is the single definition the
// HTTP layer's OpenAPI schema must mirror: an edit that carries any other value
// is a validation failure, never a silently stored string. An empty value in a
// patch clears the field; a missing review_status on a stored question means
// "never reviewed", which NeedsReview counts as pending work.
var ReviewStatuses = []string{"pending", "approved", "rejected"}

// ValidReviewStatus reports whether status is a member of ReviewStatuses.
func ValidReviewStatus(status string) bool {
	for _, allowed := range ReviewStatuses {
		if status == allowed {
			return true
		}
	}
	return false
}

// NeedsReview reports whether a question's review_status still requires a human
// decision. A missing value means "never reviewed", and only an explicit
// decision (approved or rejected) takes a question out of the pending count.
func NeedsReview(status string) bool { return status != "approved" && status != "rejected" }

// ItemPatch is one partial edit of an occurrence. A nil field was not
// submitted; every submitted field replaces the stored value. There is
// deliberately no field for kind, parent/child structure, ordering or asset
// identity: those cannot be edited through this path.
//
// ClearAnswer distinguishes "answer not submitted" (nil Answer, false) from
// "remove the answer" (true): the schema allows an absent answer but not null.
type ItemPatch struct {
	Stem         *[]ir.ContentBlock
	Options      *[]ir.Option
	Answer       *ir.Answer
	ClearAnswer  bool
	Analysis     *[]ir.ContentBlock
	Metadata     *map[string]any
	ReviewStatus *string

	// Occurrence fields, owned by the exam_items row.
	DisplayNumber *string
	Score         *float64
	AnswerSpaceMM *float64
}

// EntityChanged reports whether the patch touches the shared question entity.
func (p ItemPatch) EntityChanged() bool {
	return p.Stem != nil || p.Options != nil || p.Answer != nil || p.ClearAnswer ||
		p.Analysis != nil || p.Metadata != nil || p.ReviewStatus != nil
}

// ItemEditResult is the outcome of one successful edit.
type ItemEditResult struct {
	ItemID     uuid.UUID
	QuestionID uuid.UUID
	Revision   int64
	// Content is the occurrence as the exam now assembles it, with the
	// occurrence values projected onto it.
	Content ir.QuestionItem
}

// EditItem applies one patch to one occurrence, in one transaction, with the
// copy-on-write rule for shared question entities.
//
// Lock order is exam -> exam_item -> question, the order every writer uses.
// The referenced question row is locked before its reference count is read, so
// a concurrent ingest (which locks the questions it references before it
// inserts items) cannot attach between the count and the rewrite. A shared
// entity is never edited in place: the item is relinked to a clone, so other
// exams keep their content and their revisions.
func (a *Assembler) EditItem(ctx context.Context, ws Workspace, examID, itemID uuid.UUID, ifMatchRevision int64, patch ItemPatch) (res ItemEditResult, err error) {
	defer func() {
		if err != nil {
			err = Translate(err)
		}
	}()
	if examID == uuid.Nil || itemID == uuid.Nil {
		return ItemEditResult{}, fmt.Errorf("%w: exam and item IDs are required", ErrInvalid)
	}
	if ifMatchRevision <= 0 {
		return ItemEditResult{}, fmt.Errorf("%w: If-Match must carry a revision", ErrInvalid)
	}

	// 1. Lock the exam, then the occurrence, and prove the occurrence belongs
	//    to that exam. A mismatched pair is a 404, never a cross-exam write.
	exam, err := ws.GetExamForUpdate(ctx, pgUUID(examID))
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ItemEditResult{}, fmt.Errorf("%w: exam %s", ErrNotFound, examID)
	case err != nil:
		return ItemEditResult{}, fmt.Errorf("lock exam: %w", err)
	}
	item, err := ws.GetExamItemForUpdate(ctx, pgUUID(itemID))
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ItemEditResult{}, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	case err != nil:
		return ItemEditResult{}, fmt.Errorf("lock item: %w", err)
	}
	if item.ExamID != exam.ExamID {
		return ItemEditResult{}, fmt.Errorf("%w: item %s is not part of exam %s", ErrNotFound, itemID, examID)
	}
	if item.Revision != ifMatchRevision {
		return ItemEditResult{}, &RevisionConflictError{Resource: "item " + itemID.String(), Current: item.Revision}
	}

	// 2. Lock the question entity this occurrence currently references.
	if _, err := ws.LockQuestions(ctx, []pgtype.UUID{item.QuestionID}); err != nil {
		return ItemEditResult{}, fmt.Errorf("lock question: %w", err)
	}
	entity, err := ws.GetQuestionForUpdate(ctx, item.QuestionID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return ItemEditResult{}, fmt.Errorf("%w: question %s referenced by item %s", ErrNotFound, uuidString(item.QuestionID), itemID)
	case err != nil:
		return ItemEditResult{}, fmt.Errorf("lock question row: %w", err)
	}
	current, err := decodeQuestion(entity.ContentJson, entity.QuestionID)
	if err != nil {
		return ItemEditResult{}, err
	}

	questionID := item.QuestionID
	var orphan pgtype.UUID
	if patch.EntityChanged() {
		updated, err := applyEntityPatch(current, patch)
		if err != nil {
			return ItemEditResult{}, err
		}
		questionID, orphan, err = a.writeEntityEdit(ctx, ws, entity, current, updated)
		if err != nil {
			return ItemEditResult{}, err
		}
	} else if !patchTouchesOccurrence(patch) {
		return ItemEditResult{}, fmt.Errorf("%w: the patch has no fields", ErrInvalid)
	}

	// 3. Occurrence values live only on this row. A field the patch does not
	//    submit keeps its stored override untouched: re-deriving it from the
	//    entity would rewrite a row that did not change.
	display := item.DisplayNumberOverride
	score := item.ScoreOverride
	space := item.AnswerSpaceMmOverride
	if patch.DisplayNumber != nil {
		display = occurrenceText(current.DisplayNumber, *patch.DisplayNumber)
	}
	if patch.Score != nil {
		score = occurrenceFloat(current.Score, *patch.Score)
	}
	if patch.AnswerSpaceMM != nil {
		space = occurrenceFloat(0, *patch.AnswerSpaceMM)
	}
	updatedItem, err := ws.UpdateItem(ctx, sqlcgen.UpdateItemParams{
		ItemID:                item.ItemID,
		QuestionID:            questionID,
		DisplayNumberOverride: display,
		ScoreOverride:         score,
		AnswerSpaceMmOverride: space,
		Revision:              item.Revision,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
			// The compare-and-set lost: another writer moved the revision.
			fresh, readErr := ws.GetExamItemForUpdate(ctx, pgUUID(itemID))
			if readErr == nil {
				return ItemEditResult{}, &RevisionConflictError{Resource: "item " + itemID.String(), Current: fresh.Revision}
			}
			return ItemEditResult{}, fmt.Errorf("update item: %w", err)
		}
		return ItemEditResult{}, fmt.Errorf("update item: %w", err)
	}

	// 4. Rebuild the whole exam in this transaction. Assembly validates the
	//    schema, the semantics and that every referenced asset is registered;
	//    any failure rolls the clone, the relink and the occurrence write back.
	assembled, err := a.Assemble(ctx, ws, examID)
	if err != nil {
		return ItemEditResult{}, err
	}
	// The relink is committed, so the old entity is now truly unreferenced. The
	// NOT EXISTS guard keeps this safe if another item attached meanwhile.
	if orphan.Valid {
		if _, err := ws.DeleteQuestionIfUnreferenced(ctx, orphan); err != nil {
			return ItemEditResult{}, fmt.Errorf("delete replaced entity: %w", err)
		}
	}
	content, err := itemContent(ctx, ws, exam, item, assembled)
	if err != nil {
		return ItemEditResult{}, err
	}
	return ItemEditResult{
		ItemID:     itemID,
		QuestionID: uuid.UUID(questionID.Bytes),
		Revision:   updatedItem.Revision,
		Content:    content,
	}, nil
}

// writeEntityEdit persists an entity edit under the CoW rule and returns the
// question ID the occurrence must point at. When the edit lands on an existing
// equal entity, the now-possibly-orphaned old entity is returned so the caller
// can delete it after the relink (deleting before the relink is a no-op: this
// item still references it).
func (a *Assembler) writeEntityEdit(ctx context.Context, ws Workspace, entity sqlcgen.Question, current, updated ir.QuestionItem) (pgtype.UUID, pgtype.UUID, error) {
	fp, err := QuestionFingerprint(updated)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// Equal semantics under the same version may already exist as another
	// entity: reuse it instead of creating a third copy of the same content.
	candidates, err := ws.ListQuestionsByFingerprintForUpdate(ctx, sqlcgen.ListQuestionsByFingerprintForUpdateParams{
		ContentFingerprint: fp, FingerprintVersion: FingerprintVersion,
	})
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("lookup fingerprint: %w", err)
	}
	for _, c := range candidates {
		if c.QuestionID == entity.QuestionID {
			continue
		}
		content, err := decodeQuestion(c.ContentJson, c.QuestionID)
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, err
		}
		if sameQuestionContent(content, updated) {
			return c.QuestionID, entity.QuestionID, nil
		}
	}

	refs, err := ws.CountQuestionReferences(ctx, entity.QuestionID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("count references: %w", err)
	}
	contentJSON, err := marshalEntity(updated)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	points, difficulty, err := projectMetadata(updated.Metadata)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if refs > 1 {
		// Shared: clone the edited entity and relink only this occurrence.
		row, err := ws.InsertQuestion(ctx, sqlcgen.InsertQuestionParams{
			Subject:            entity.Subject,
			GradeLevel:         entity.GradeLevel,
			Kind:               updated.Kind,
			QuestionType:       textOrNil(updated.QuestionType),
			KnowledgePoints:    points,
			Difficulty:         difficulty,
			ContentFingerprint: fp,
			FingerprintVersion: FingerprintVersion,
			ContentJson:        contentJSON,
		})
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("clone question: %w", err)
		}
		return row.QuestionID, pgtype.UUID{}, nil
	}
	// Exclusive: the edit is visible only in this occurrence, so the entity
	// can be rewritten in place.
	row, err := ws.UpdateQuestion(ctx, sqlcgen.UpdateQuestionParams{
		QuestionID:         entity.QuestionID,
		Subject:            entity.Subject,
		GradeLevel:         entity.GradeLevel,
		Kind:               updated.Kind,
		QuestionType:       textOrNil(updated.QuestionType),
		KnowledgePoints:    points,
		Difficulty:         difficulty,
		ContentFingerprint: fp,
		FingerprintVersion: FingerprintVersion,
		ContentJson:        contentJSON,
	})
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("update question: %w", err)
	}
	return row.QuestionID, pgtype.UUID{}, nil
}

// applyEntityPatch merges the submitted entity fields onto the stored content.
// Occurrence fields are left untouched: they are written to the item, never to
// the entity.
func applyEntityPatch(current ir.QuestionItem, patch ItemPatch) (ir.QuestionItem, error) {
	updated := current
	// The entity never carries an occurrence answer space; a legacy value is
	// dropped here so the clone or update writes the v2 shape.
	updated.AnswerSpaceMM = 0
	if patch.Stem != nil {
		updated.Stem = append([]ir.ContentBlock(nil), (*patch.Stem)...)
	}
	if patch.Options != nil {
		updated.Options = append([]ir.Option(nil), (*patch.Options)...)
	}
	if patch.ClearAnswer {
		updated.Answer = nil
	} else if patch.Answer != nil {
		answer := *patch.Answer
		updated.Answer = &answer
	}
	if patch.Analysis != nil {
		updated.Analysis = append([]ir.ContentBlock(nil), (*patch.Analysis)...)
	}
	if patch.Metadata != nil {
		if *patch.Metadata == nil {
			updated.Metadata = nil
		} else {
			updated.Metadata = make(map[string]any, len(*patch.Metadata))
			for key, value := range *patch.Metadata {
				updated.Metadata[key] = value
			}
		}
	}
	if patch.ReviewStatus != nil {
		if *patch.ReviewStatus != "" && !ValidReviewStatus(*patch.ReviewStatus) {
			return ir.QuestionItem{}, fmt.Errorf("%w: provenance.review_status %q is not one of %v",
				ErrInvalid, *patch.ReviewStatus, ReviewStatuses)
		}
		if *patch.ReviewStatus == "" {
			if len(updated.Provenance) > 0 {
				provenance := make(map[string]any, len(updated.Provenance))
				for key, value := range updated.Provenance {
					if key != "review_status" {
						provenance[key] = value
					}
				}
				if len(provenance) == 0 {
					provenance = nil
				}
				updated.Provenance = provenance
			}
		} else {
			provenance := make(map[string]any, len(updated.Provenance)+1)
			for key, value := range updated.Provenance {
				provenance[key] = value
			}
			provenance["review_status"] = *patch.ReviewStatus
			updated.Provenance = provenance
		}
	}
	return updated, nil
}

// marshalEntity serializes a question for content_json, without the
// occurrence-only top-level answer space.
func marshalEntity(q ir.QuestionItem) ([]byte, error) {
	content, err := json.Marshal(entityContent(q))
	if err != nil {
		return nil, fmt.Errorf("%w: question content: %v", ErrInvalid, err)
	}
	return content, nil
}

// patchTouchesOccurrence reports whether the patch writes the item row.
func patchTouchesOccurrence(p ItemPatch) bool {
	return p.DisplayNumber != nil || p.Score != nil || p.AnswerSpaceMM != nil
}

// itemContent finds one occurrence's content in an assembled exam. The item's
// section row gives the section's IR ID and ordinal; the assembled document is
// in IR order, so both indices are direct.
func itemContent(ctx context.Context, ws Workspace, exam sqlcgen.Exam, item sqlcgen.GetExamItemForUpdateRow, assembled ir.ExamDocumentIR) (ir.QuestionItem, error) {
	sections, err := ws.ListSections(ctx, exam.ExamID)
	if err != nil {
		return ir.QuestionItem{}, fmt.Errorf("read sections: %w", err)
	}
	var irSectionID string
	var sectionOrdinal int32 = -1
	for _, section := range sections {
		if section.SectionID == item.SectionID {
			irSectionID, sectionOrdinal = section.IrSectionID, section.Ordinal
			break
		}
	}
	if sectionOrdinal < 0 || int(sectionOrdinal) >= len(assembled.Sections) {
		return ir.QuestionItem{}, fmt.Errorf("%w: edited item's section is missing from the assembled exam", ErrInvalid)
	}
	questions := assembled.Sections[sectionOrdinal].Questions
	if int(item.Ordinal) >= len(questions) {
		return ir.QuestionItem{}, fmt.Errorf("%w: edited item's ordinal is missing from the assembled exam", ErrInvalid)
	}
	if assembled.Sections[sectionOrdinal].SectionID != irSectionID {
		return ir.QuestionItem{}, fmt.Errorf("%w: assembled section order disagrees with the section rows", ErrInvalid)
	}
	return questions[item.Ordinal], nil
}

// SetActiveVariant switches one asset's served variant under a row lock and a
// revision compare-and-set. Only a variant with a registered, valid key and
// digest can become active; a switch never rewrites either variant's key or
// digest.
func SetActiveVariant(ctx context.Context, ws Workspace, assetID string, variant storage.Variant, ifMatchRevision int64) (rec storage.AssetRecord, err error) {
	defer func() {
		if err != nil {
			err = Translate(err)
		}
	}()
	if variant != storage.VariantRaw && variant != storage.VariantClean {
		return storage.AssetRecord{}, fmt.Errorf("%w: variant must be raw or clean", ErrInvalid)
	}
	if ifMatchRevision <= 0 {
		return storage.AssetRecord{}, fmt.Errorf("%w: If-Match must carry a revision", ErrInvalid)
	}
	row, err := ws.GetAssetForUpdate(ctx, assetID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return storage.AssetRecord{}, fmt.Errorf("%w: asset %s", ErrNotFound, assetID)
	case err != nil:
		return storage.AssetRecord{}, fmt.Errorf("lock asset: %w", err)
	}
	if row.Revision != ifMatchRevision {
		return storage.AssetRecord{}, &RevisionConflictError{Resource: "asset " + assetID, Current: row.Revision}
	}
	// The target variant must be servable before it can be selected. This is
	// what keeps a legacy raw companion without its own digest unswitchable.
	if _, _, err := AssetRecordFromRow(row).Select(variant); err != nil {
		return storage.AssetRecord{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	updated, err := ws.SetAssetActiveVariant(ctx, sqlcgen.SetAssetActiveVariantParams{
		AssetID:       assetID,
		ActiveVariant: string(variant),
		Revision:      row.Revision,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
			fresh, readErr := ws.GetAssetForUpdate(ctx, assetID)
			if readErr == nil {
				return storage.AssetRecord{}, &RevisionConflictError{Resource: "asset " + assetID, Current: fresh.Revision}
			}
		}
		return storage.AssetRecord{}, fmt.Errorf("switch asset variant: %w", err)
	}
	return AssetRecordFromRow(updated), nil
}
