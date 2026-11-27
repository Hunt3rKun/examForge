package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
	"examforge/internal/store/storetest"
)

// editItem runs one edit in a simulated transaction, the way the repository
// runs it against PostgreSQL.
func editItem(t *testing.T, a *store.Assembler, db *storetest.MemDB, examID, itemID uuid.UUID, revision int64, patch store.ItemPatch) (store.ItemEditResult, error) {
	t.Helper()
	var res store.ItemEditResult
	err := db.InTx(t.Context(), func(ctx context.Context) error {
		var err error
		res, err = a.EditItem(ctx, db, examID, itemID, revision, patch)
		return err
	})
	return res, err
}

func itemsOf(t *testing.T, db *storetest.MemDB, examID uuid.UUID) []sqlcgen.ExamItem {
	t.Helper()
	items, err := db.ListExamItems(t.Context(), pgUUIDOf(examID))
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func sectionsOf(t *testing.T, db *storetest.MemDB, examID uuid.UUID) []sqlcgen.ExamSection {
	t.Helper()
	sections, err := db.ListSections(t.Context(), pgUUIDOf(examID))
	if err != nil {
		t.Fatal(err)
	}
	return sections
}

func pgUUIDOf(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func entityID(id pgtype.UUID) uuid.UUID { return uuid.UUID(id.Bytes) }

func stemText(q ir.QuestionItem) string {
	if len(q.Stem) == 0 {
		return ""
	}
	return q.Stem[0].Markdown
}

func textPatch(markdown string) store.ItemPatch {
	stem := []ir.ContentBlock{{Type: "text", Markdown: markdown}}
	return store.ItemPatch{Stem: &stem}
}

// TestEditSharedQuestionClonesAndRelinksOnlyThisItem is the copy-on-write
// contract: editing one occurrence of a shared entity must leave every other
// exam untouched, with its own item revisions intact.
func TestEditSharedQuestionClonesAndRelinksOnlyThisItem(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	other := clone(t, source)
	other.ExamID = "exam_other_owner"
	persist(t, a, db, other, activeJob(t, db, sourceSHA))

	examA := recExamID(t, db, source.ExamID)
	examB := recExamID(t, db, other.ExamID)
	target := itemsOf(t, db, examA)[0]
	before := len(db.Questions())
	if before != 3 {
		t.Fatalf("shared persist wrote %d entities, want 3", before)
	}

	res, err := editItem(t, a, db, examA, uuid.UUID(target.ItemID.Bytes), target.Revision, textPatch("改后的题干"))
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if res.QuestionID == entityID(target.QuestionID) {
		t.Fatal("a shared entity was edited in place instead of cloned")
	}
	if res.Revision != target.Revision+1 {
		t.Errorf("item revision = %d, want %d", res.Revision, target.Revision+1)
	}
	if got := len(db.Questions()); got != before+1 {
		t.Errorf("entity count = %d, want %d (one clone)", got, before+1)
	}

	// The other exam keeps the original content and its own item revision.
	assembledB, err := a.Assemble(t.Context(), db, examB)
	if err != nil {
		t.Fatal(err)
	}
	if got := stemText(assembledB.Sections[0].Questions[0]); got != stemText(source.Sections[0].Questions[0]) {
		t.Errorf("the other exam's stem changed to %q", got)
	}
	for _, item := range itemsOf(t, db, examB) {
		if item.Revision != 1 {
			t.Errorf("the other exam's item revision = %d, want 1", item.Revision)
		}
	}

	assembledA, err := a.Assemble(t.Context(), db, examA)
	if err != nil {
		t.Fatal(err)
	}
	if got := stemText(assembledA.Sections[0].Questions[0]); got != "改后的题干" {
		t.Errorf("edited stem = %q", got)
	}
}

// TestEditExclusiveQuestionUpdatesInPlace: a question with exactly one
// occurrence is rewritten, not copied.
func TestEditExclusiveQuestionUpdatesInPlace(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]

	res, err := editItem(t, a, db, examID, uuid.UUID(target.ItemID.Bytes), target.Revision, textPatch("独占题改写"))
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if res.QuestionID != entityID(target.QuestionID) {
		t.Errorf("exclusive entity was cloned: %s -> %s", entityID(target.QuestionID), res.QuestionID)
	}
	if got := len(db.Questions()); got != 3 {
		t.Errorf("entity count = %d, want 3", got)
	}
	assembled, err := a.Assemble(t.Context(), db, examID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stemText(assembled.Sections[0].Questions[0]); got != "独占题改写" {
		t.Errorf("stem = %q", got)
	}
}

// TestEditOccurrenceFieldsStayOnTheItem: display number, score and answer
// space are exam_items columns; the shared entity must not change at all.
func TestEditOccurrenceFieldsStayOnTheItem(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]

	before := db.Questions()
	display, score, space := "2b", 7.5, 18.0
	res, err := editItem(t, a, db, examID, uuid.UUID(target.ItemID.Bytes), target.Revision, store.ItemPatch{
		DisplayNumber: &display, Score: &score, AnswerSpaceMM: &space,
	})
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if res.QuestionID != entityID(target.QuestionID) {
		t.Error("an occurrence-only edit replaced the entity")
	}
	if res.Revision != 2 {
		t.Errorf("revision = %d, want 2", res.Revision)
	}
	after := db.Questions()
	if len(after) != len(before) {
		t.Fatalf("entity count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if string(before[i].ContentJson) != string(after[i].ContentJson) {
			t.Errorf("question %s content_json changed on an occurrence-only edit", before[i].QuestionID)
		}
		// The top-level answer space is an occurrence value and must never be
		// stored on the entity. A composite's children keep theirs: they have no
		// exam_items row of their own.
		var stored ir.QuestionItem
		if err := json.Unmarshal(after[i].ContentJson, &stored); err != nil {
			t.Fatalf("decode question %s: %v", after[i].QuestionID, err)
		}
		if stored.AnswerSpaceMM != 0 {
			t.Errorf("question %s content_json carries answer_space_mm %v", after[i].QuestionID, stored.AnswerSpaceMM)
		}
	}
	assembled, err := a.Assemble(t.Context(), db, examID)
	if err != nil {
		t.Fatal(err)
	}
	got := assembled.Sections[0].Questions[0]
	if got.DisplayNumber != display || got.Score != score || got.AnswerSpaceMM != space {
		t.Errorf("assembled occurrence = %+v, want display %q score %v space %v", got, display, score, space)
	}
}

// TestEditRejectsStaleIfMatch: the revision is the concurrency token, and a
// stale one reports the current revision instead of silently winning.
func TestEditRejectsStaleIfMatch(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]

	_, err := editItem(t, a, db, examID, uuid.UUID(target.ItemID.Bytes), 99, textPatch("stale"))
	var conflict *store.RevisionConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want a revision conflict", err)
	}
	if conflict.Current != target.Revision {
		t.Errorf("conflict reports revision %d, want %d", conflict.Current, target.Revision)
	}
	assembled, err := a.Assemble(t.Context(), db, examID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stemText(assembled.Sections[0].Questions[0]); got == "stale" {
		t.Error("a stale edit was applied")
	}
}

// TestEditLocksQuestionBeforeCountingReferences proves the reference protocol:
// an ingest that attaches between the lock and the count is seen, so the edit
// clones instead of rewriting an entity another exam now shares.
func TestEditLocksQuestionBeforeCountingReferences(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	other := clone(t, source)
	other.ExamID = "exam_attaching"
	persist(t, a, db, other, activeJob(t, db, sourceSHA))

	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]
	attachSection := sectionsOf(t, db, recExamID(t, db, other.ExamID))[0]

	// The hook models a concurrent ingest whose attach committed just before
	// the lock was granted. Only an edit that locks first sees it.
	var attached bool
	db.OnLockQuestions = func(id pgtype.UUID) {
		if attached || id != target.QuestionID {
			return
		}
		attached = true
		if err := db.InsertItem(t.Context(), sqlcgen.InsertItemParams{
			SectionID: attachSection.SectionID, QuestionID: target.QuestionID, Ordinal: 99,
		}); err != nil {
			t.Fatalf("attach during lock: %v", err)
		}
	}

	res, err := editItem(t, a, db, examID, uuid.UUID(target.ItemID.Bytes), target.Revision, textPatch("竞争下改写"))
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if !attached {
		t.Fatal("the edit never locked the question row")
	}
	if res.QuestionID == entityID(target.QuestionID) {
		t.Fatal("the edit counted references before locking and rewrote a shared entity in place")
	}
	locked := false
	for _, id := range db.LockLog() {
		if id == target.QuestionID {
			locked = true
		}
	}
	if !locked {
		t.Error("the question row was never locked")
	}
}

// TestEditSameExamDuplicateTargetsOneItem: two occurrences of one entity in one
// exam are edited by item_id; the sibling keeps the original content.
func TestEditSameExamDuplicateTargetsOneItem(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	section := sectionsOf(t, db, examID)[0]
	original := itemsOf(t, db, examID)[0]
	if err := db.InsertItem(t.Context(), sqlcgen.InsertItemParams{
		SectionID: section.SectionID, QuestionID: original.QuestionID, Ordinal: 2,
	}); err != nil {
		t.Fatal(err)
	}
	duplicate := itemsOf(t, db, examID)[2]
	if duplicate.QuestionID != original.QuestionID {
		t.Fatal("fixture does not share one entity between two items")
	}

	res, err := editItem(t, a, db, examID, uuid.UUID(duplicate.ItemID.Bytes), duplicate.Revision, textPatch("只改第二条"))
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	if res.QuestionID == entityID(original.QuestionID) {
		t.Fatal("the shared entity was rewritten in place")
	}
	assembled, err := a.Assemble(t.Context(), db, examID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stemText(assembled.Sections[0].Questions[0]); got != stemText(source.Sections[0].Questions[0]) {
		t.Errorf("the sibling occurrence changed to %q", got)
	}
	if got := stemText(assembled.Sections[0].Questions[2]); got != "只改第二条" {
		t.Errorf("the edited occurrence = %q", got)
	}
}

// TestEditReusesEqualEntityAndDropsTheOrphan: an edit that reproduces content
// already stored as another entity relinks to it instead of adding a duplicate,
// and the now-unreferenced old entity is removed.
func TestEditReusesEqualEntityAndDropsTheOrphan(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	pending := clone(t, source)
	pending.ExamID = "exam_pending_review"
	pending.Sections[0].Questions[0].Provenance["review_status"] = "pending"
	persist(t, a, db, pending, activeJob(t, db, sourceSHA))

	examA := recExamID(t, db, source.ExamID)
	examB := recExamID(t, db, pending.ExamID)
	target := itemsOf(t, db, examA)[0]
	if got := len(db.Questions()); got != 4 {
		t.Fatalf("entities = %d, want 4", got)
	}

	status := "pending"
	res, err := editItem(t, a, db, examA, uuid.UUID(target.ItemID.Bytes), target.Revision, store.ItemPatch{ReviewStatus: &status})
	if err != nil {
		t.Fatalf("EditItem: %v", err)
	}
	otherEntity := itemsOf(t, db, examB)[0].QuestionID
	if res.QuestionID != entityID(otherEntity) {
		t.Errorf("edit created a duplicate entity instead of reusing %s", entityID(otherEntity))
	}
	if got := len(db.Questions()); got != 3 {
		t.Errorf("entities = %d, want 3 (the orphan was not removed)", got)
	}
	assembled, err := a.Assemble(t.Context(), db, examA)
	if err != nil {
		t.Fatal(err)
	}
	if got := assembled.Sections[0].Questions[0].Provenance["review_status"]; got != "pending" {
		t.Errorf("review_status = %v, want pending", got)
	}
}

// TestEditRejectsInvalidContentAndRollsBackEverything: the transaction is
// all-or-nothing. An unregistered asset reference fails assembly after the
// clone and the relink, and the rollback restores both.
func TestEditRejectsInvalidContentAndRollsBackEverything(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]
	beforeQuestions := len(db.Questions())
	beforeContent := map[string]string{}
	for _, q := range db.Questions() {
		beforeContent[uuid.UUID(q.QuestionID.Bytes).String()] = string(q.ContentJson)
	}

	stem := []ir.ContentBlock{
		{Type: "text", Markdown: "题干"},
		{Type: "image_ref", AssetID: "ast_unregistered00000000000001", LayoutHint: "center"},
	}
	_, err := editItem(t, a, db, examID, uuid.UUID(target.ItemID.Bytes), target.Revision, store.ItemPatch{Stem: &stem})
	if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("err = %v, want a validation failure", err)
	}
	if got := len(db.Questions()); got != beforeQuestions {
		t.Errorf("entities = %d after rollback, want %d", got, beforeQuestions)
	}
	for _, q := range db.Questions() {
		id := uuid.UUID(q.QuestionID.Bytes).String()
		if beforeContent[id] != string(q.ContentJson) {
			t.Errorf("question %s content changed despite the rollback", id)
		}
	}
	if got := itemsOf(t, db, examID)[0].Revision; got != target.Revision {
		t.Errorf("item revision = %d after rollback, want %d", got, target.Revision)
	}
	if _, err := a.Assemble(t.Context(), db, examID); err != nil {
		t.Errorf("the exam no longer assembles after the rollback: %v", err)
	}
}

// TestEditValidatesReviewStatusAndEmptyPatches keeps the transport contract
// honest: the enum has one definition and an empty patch is a client error.
func TestEditValidatesReviewStatusAndEmptyPatches(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	examID := recExamID(t, db, source.ExamID)
	target := itemsOf(t, db, examID)[0]
	itemID := uuid.UUID(target.ItemID.Bytes)

	status := "maybe"
	if _, err := editItem(t, a, db, examID, itemID, target.Revision, store.ItemPatch{ReviewStatus: &status}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("unknown review_status err = %v, want store.ErrInvalid", err)
	}
	if _, err := editItem(t, a, db, examID, itemID, target.Revision, store.ItemPatch{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty patch err = %v, want store.ErrInvalid", err)
	}
	if _, err := editItem(t, a, db, examID, itemID, 0, textPatch("x")); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("missing If-Match err = %v, want store.ErrInvalid", err)
	}
}

// TestEditRejectsAnItemOfAnotherExam: the item must belong to the exam on the
// path, so a mismatched pair can never write across exams.
func TestEditRejectsAnItemOfAnotherExam(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	other := clone(t, source)
	other.ExamID = "exam_other_pair"
	persist(t, a, db, other, activeJob(t, db, sourceSHA))

	examA := recExamID(t, db, source.ExamID)
	foreign := itemsOf(t, db, recExamID(t, db, other.ExamID))[0]
	_, err := editItem(t, a, db, examA, uuid.UUID(foreign.ItemID.Bytes), foreign.Revision, textPatch("跨卷"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

// TestSetActiveVariantSwitchesLosslessly: the selector moves and the revision
// advances; neither variant's key or digest is touched.
func TestSetActiveVariantSwitchesLosslessly(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam, ups := cleanedFixture(t)
	jobID := activeJob(t, db, sourceSHA)
	if _, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
		JobID: jobID, SourceSHA256: sourceSHA, Assets: ups,
	}); err != nil {
		t.Fatal(err)
	}
	id := ups[0].AssetID
	original := db.Assets()[0]
	if original.ActiveVariant != string(storage.VariantClean) || original.Revision != 1 {
		t.Fatalf("initial row = %+v", original)
	}

	rec, err := store.SetActiveVariant(t.Context(), db, id, storage.VariantRaw, 1)
	if err != nil {
		t.Fatalf("switch to raw: %v", err)
	}
	if rec.ActiveVariant != storage.VariantRaw || rec.Revision != 2 {
		t.Fatalf("switched record = %+v", rec)
	}
	row := db.Assets()[0]
	if row.StorageKey != original.StorageKey || row.Sha256 != original.Sha256 ||
		row.RawStorageKey != original.RawStorageKey || row.RawSha256 != original.RawSha256 ||
		row.CleanStorageKey != original.CleanStorageKey || row.CleanSha256 != original.CleanSha256 {
		t.Fatalf("a variant switch rewrote a key or digest: %+v", row)
	}

	rec, err = store.SetActiveVariant(t.Context(), db, id, storage.VariantClean, 2)
	if err != nil {
		t.Fatalf("switch back to clean: %v", err)
	}
	if rec.ActiveVariant != storage.VariantClean || rec.Revision != 3 {
		t.Fatalf("switched back record = %+v", rec)
	}
	if got := rec.Available(); len(got) != 2 {
		t.Errorf("available variants = %v, want raw and clean", got)
	}

	// A stale If-Match never wins.
	_, err = store.SetActiveVariant(t.Context(), db, id, storage.VariantRaw, 1)
	var conflict *store.RevisionConflictError
	if !errors.As(err, &conflict) || conflict.Current != 3 {
		t.Fatalf("stale switch err = %v, want a conflict reporting revision 3", err)
	}
	if db.Assets()[0].ActiveVariant != string(storage.VariantClean) {
		t.Error("a stale switch changed the selector")
	}
}

// TestSetActiveVariantRejectsUnavailableVariants: a raw_cropped asset has no
// clean variant, and a legacy raw companion without its own digest cannot be
// verified, so neither can be selected.
func TestSetActiveVariantRejectsUnavailableVariants(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam := loadGolden(t) // raw_cropped
	if _, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
		JobID: activeJob(t, db, sourceSHA), SourceSHA256: sourceSHA, Assets: uploads(t, exam),
	}); err != nil {
		t.Fatal(err)
	}
	rawID := db.Assets()[0].AssetID
	if _, err := store.SetActiveVariant(t.Context(), db, rawID, storage.VariantClean, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("raw_cropped clean switch err = %v, want store.ErrNotFound", err)
	}
	if _, err := store.SetActiveVariant(t.Context(), db, "ast_unknown00000000000000000001", storage.VariantRaw, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown asset err = %v, want store.ErrNotFound", err)
	}
	if _, err := store.SetActiveVariant(t.Context(), db, rawID, storage.VariantActive, 1); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("active selector err = %v, want store.ErrInvalid", err)
	}

	// A legacy cleaned row whose raw companion has no digest: clean is the only
	// servable variant, so raw cannot become active.
	legacy := storetest.New()
	if _, err := legacy.InsertAsset(t.Context(), sqlcgen.InsertAssetParams{
		AssetID: "ast_legacy0000000000000000001", MediaType: "image/png",
		StorageKey: "assets/clean/ast_legacy0000000000000000001.png",
		RawStorageKey: pgtype.Text{
			String: "assets/raw/ast_legacy0000000000000000001.png", Valid: true,
		},
		Status:        "cleaned",
		Sha256:        strings.Repeat("ab", 32),
		ActiveVariant: string(storage.VariantClean),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetActiveVariant(t.Context(), legacy, "ast_legacy0000000000000000001", storage.VariantRaw, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("legacy raw switch err = %v, want store.ErrNotFound", err)
	}
	if _, err := store.SetActiveVariant(t.Context(), legacy, "ast_legacy0000000000000000001", storage.VariantClean, 1); err != nil {
		t.Errorf("legacy clean switch err = %v, want success", err)
	}
}
