package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
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

const (
	examSchema   = "../../schemas/exam-ir-v1.json"
	layoutSchema = "../../schemas/layout-spec-v1.json"
	goldenIR     = "../../testdata/golden/ir/sample.json"
	goldenAsset  = "../../testdata/golden/assets/sample_fig.png"
)

func newAssembler(t *testing.T) *store.Assembler {
	t.Helper()
	validator, err := ir.NewValidator(examSchema, layoutSchema)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	a, err := store.NewAssembler(validator)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func loadGolden(t *testing.T) ir.ExamDocumentIR {
	t.Helper()
	data, err := os.ReadFile(goldenIR)
	if err != nil {
		t.Fatal(err)
	}
	var exam ir.ExamDocumentIR
	if err := json.Unmarshal(data, &exam); err != nil {
		t.Fatal(err)
	}
	return exam
}

// clone deep-copies an exam through JSON.
func clone(t *testing.T, exam ir.ExamDocumentIR) ir.ExamDocumentIR {
	t.Helper()
	data, err := json.Marshal(exam)
	if err != nil {
		t.Fatal(err)
	}
	var out ir.ExamDocumentIR
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// activeJob creates a job that already sits in validating, as the worker does
// before it persists.
func activeJob(t *testing.T, db *storetest.MemDB, sourceSHA string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	jobID := pgtype.UUID{Bytes: id, Valid: true}
	ctx := t.Context()
	if _, err := db.InsertIngestJob(ctx, sqlcgen.InsertIngestJobParams{JobID: jobID, SourcePdfKey: "uploads/" + id.String() + "/source.pdf", SourceFileSha256: sourceSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimIngestJob(ctx, sqlcgen.ClaimIngestJobParams{JobID: jobID, States: []string{store.JobQueued}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MoveIngestJob(ctx, sqlcgen.MoveIngestJobParams{JobID: jobID, State: store.JobValidating, FromStates: []string{store.JobParsing}}); err != nil {
		t.Fatal(err)
	}
	return id
}

var sourceSHA = strings.Repeat("ab", 32)

func persist(t *testing.T, a *store.Assembler, db *storetest.MemDB, exam ir.ExamDocumentIR, jobID uuid.UUID) store.ExamRecord {
	t.Helper()
	rec, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
		JobID: jobID, SourcePDFKey: "uploads/" + jobID.String() + "/source.pdf",
		SourceSHA256: sourceSHA, Assets: uploads(t, exam),
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return rec
}

// assertSemanticallyEqual compares two Exam IR documents by value. Object
// key order never matters; arrays are order sensitive. The only allowed
// difference is the top-level question_id: the original source value is
// ignored and the assembled value must be the question entity UUID. Asset
// URIs are locators and are compared through the resolver-neutral scheme.
func assertSemanticallyEqual(t *testing.T, source, assembled ir.ExamDocumentIR) {
	t.Helper()
	left, right := clone(t, source), clone(t, assembled)
	entityIDs := map[string]string{}
	for si := range left.Sections {
		for qi := range left.Sections[si].Questions {
			got := right.Sections[si].Questions[qi].QuestionID
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("sections[%d].questions[%d].question_id %q is not an entity UUID", si, qi, got)
			}
			entityIDs[fmt.Sprintf("%d/%d", si, qi)] = got
			left.Sections[si].Questions[qi].QuestionID = ""
			right.Sections[si].Questions[qi].QuestionID = ""
		}
	}
	for id, asset := range right.Assets {
		if want := store.AssetURIScheme + id; asset.URI != want {
			t.Errorf("asset %s URI = %q, want %q", id, asset.URI, want)
		}
		asset.URI, asset.RawURI = left.Assets[id].URI, left.Assets[id].RawURI
		right.Assets[id] = asset
	}
	if !reflect.DeepEqual(left, right) {
		t.Errorf("assembled exam differs from the source:\n source: %s\n assembled: %s", pretty(left), pretty(right))
	}
	// Every occurrence of one source question maps to one entity.
	if len(entityIDs) != questionCount(left) {
		t.Errorf("entity id map has %d entries for %d questions", len(entityIDs), questionCount(left))
	}
}

func questionCount(exam ir.ExamDocumentIR) int {
	n := 0
	for _, section := range exam.Sections {
		n += len(section.Questions)
	}
	return n
}

func pretty(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func TestPersistAssembleGoldenRoundTrip(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	jobID := activeJob(t, db, sourceSHA)

	rec := persist(t, a, db, source, jobID)
	if rec.Sections != 2 || rec.Items != 3 || rec.Questions != 3 {
		t.Errorf("record = %+v, want 2 sections, 3 items, 3 questions", rec)
	}
	assembled, err := a.Assemble(t.Context(), db, rec.ExamID)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// The result passes the full schema and semantic validation on its own.
	validator, _ := ir.NewValidator(examSchema, layoutSchema)
	if err := validator.ValidateExamDTO(assembled); err != nil {
		t.Fatalf("assembled exam is invalid: %v", err)
	}
	assertSemanticallyEqual(t, source, assembled)

	// The job is persisted and links to the exam.
	jobs := db.JobRows()
	if len(jobs) != 1 || jobs[0].State != store.JobPersisted || jobs[0].ExamID != (pgtype.UUID{Bytes: rec.ExamID, Valid: true}) {
		t.Errorf("job after persist = %+v", jobs[0])
	}
	// Metadata projections were written from the IR.
	question := questionBySourceID(t, db, "q_choice_1")
	if question.Subject != source.Metadata.Subject || question.GradeLevel.String != source.Metadata.Grade ||
		question.Kind != "single" || question.QuestionType.String != "single_choice" ||
		question.FingerprintVersion != store.FingerprintVersion || len(question.ContentFingerprint) != 64 {
		t.Errorf("question projection = %+v", question)
	}
	// content_json keeps the source question_id, metadata and provenance.
	var content ir.QuestionItem
	if err := json.Unmarshal(question.ContentJson, &content); err != nil {
		t.Fatal(err)
	}
	if content.QuestionID != "q_choice_1" || content.Provenance["review_status"] != "approved" || len(content.Stem) != 2 {
		t.Errorf("stored content = %s", question.ContentJson)
	}
	// The composite keeps its children recursively.
	var composite ir.QuestionItem
	for _, q := range db.Questions() {
		if q.Kind == "composite" {
			json.Unmarshal(q.ContentJson, &composite)
		}
	}
	if len(composite.Children) != 2 || composite.Children[1].Answer == nil || len(composite.Children[1].Answer.Detail) != 1 {
		t.Errorf("composite children were not preserved: %s", pretty(composite))
	}
	// The asset row carries the IR metadata and the uploaded digest.
	asset := db.Assets()
	if len(asset) != 1 || asset[0].Caption.String != "函数示意图" || asset[0].Sha256 != storage.HashBytes(mustRead(t, goldenAsset)) ||
		asset[0].AspectRatio.Float64 != 1.25 || string(asset[0].SourceBbox) != `{"page":1,"rect":[0.1,0.2,0.4,0.5]}` {
		t.Errorf("asset row = %+v", asset)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReplayPersistsSameExamWithoutDuplicates(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	jobID := activeJob(t, db, sourceSHA)
	first := persist(t, a, db, source, jobID)

	// A persisted job is never persisted twice by the worker (it is a no-op
	// replay). Re-open the job to exercise the replace path.
	job := pgtype.UUID{Bytes: jobID, Valid: true}
	if _, err := db.MoveIngestJob(t.Context(), sqlcgen.MoveIngestJobParams{JobID: job, State: store.JobValidating, FromStates: []string{store.JobPersisted}}); err != nil {
		t.Fatal(err)
	}
	again := persist(t, a, db, clone(t, source), jobID)
	if again.ExamID != first.ExamID {
		t.Errorf("replay produced exam %s, want the existing %s", again.ExamID, first.ExamID)
	}
	if n := len(db.Exams()); n != 1 {
		t.Errorf("%d exams after replay, want 1", n)
	}
	if n := len(db.Questions()); n != 3 {
		t.Errorf("%d questions after replay, want 3", n)
	}
	if n := db.CountItems(); n != 3 {
		t.Errorf("%d items after replay, want 3", n)
	}
	assembled, err := a.Assemble(t.Context(), db, first.ExamID)
	if err != nil {
		t.Fatalf("Assemble after replay: %v", err)
	}
	assertSemanticallyEqual(t, source, assembled)
}

func TestQuestionIDIsEntityUUIDAndOverridesFollowOccurrence(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	first := loadGolden(t)
	firstJob := activeJob(t, db, sourceSHA)
	firstRec := persist(t, a, db, first, firstJob)

	// A second exam reuses the same questions with other display numbers and
	// one other score. Same source bytes in another job are allowed.
	second := clone(t, first)
	second.ExamID = "exam_math_2026_mock_02"
	second.Metadata.Title = "另一份试卷"
	var counter int
	for si := range second.Sections {
		for qi := range second.Sections[si].Questions {
			counter++
			second.Sections[si].Questions[qi].DisplayNumber = fmt.Sprintf("%d", counter+10)
			second.Sections[si].Questions[qi].QuestionID = "upstream-id-must-not-be-the-key"
		}
	}
	second.Sections[0].Questions[0].Score = 8
	secondJob := activeJob(t, db, sourceSHA)
	secondRec := persist(t, a, db, second, secondJob)

	if secondRec.ExamID == firstRec.ExamID {
		t.Error("two jobs produced one exam")
	}
	if n := len(db.Exams()); n != 2 {
		t.Errorf("%d exams, want 2 with equal source SHA", n)
	}
	// The questions are shared: one entity each, stable across both exams.
	if n := len(db.Questions()); n != 3 {
		t.Fatalf("%d questions after two exams, want 3 shared entities", n)
	}
	for _, rec := range []store.ExamRecord{firstRec, secondRec} {
		assembled, err := a.Assemble(t.Context(), db, rec.ExamID)
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		if n := len(assembled.Sections); n != 2 {
			t.Errorf("exam %s has %d sections", rec.ExamID, n)
		}
	}
	firstOut, _ := a.Assemble(t.Context(), db, firstRec.ExamID)
	secondOut, err := a.Assemble(t.Context(), db, secondRec.ExamID)
	if err != nil {
		t.Fatal(err)
	}
	// Section/item order and both overrides are exact for the second exam.
	for si := range secondOut.Sections {
		for qi := range secondOut.Sections[si].Questions {
			got := secondOut.Sections[si].Questions[qi]
			want := second.Sections[si].Questions[qi]
			if got.DisplayNumber != want.DisplayNumber || got.Score != want.Score {
				t.Errorf("second exam question %d/%d = %q %v, want %q %v", si, qi, got.DisplayNumber, got.Score, want.DisplayNumber, want.Score)
			}
			// The entity UUID is identical in both exams.
			if got.QuestionID != firstOut.Sections[si].Questions[qi].QuestionID {
				t.Errorf("question %d/%d has different entity UUIDs across exams", si, qi)
			}
			if _, err := uuid.Parse(got.QuestionID); err != nil {
				t.Errorf("question_id %q is not an entity UUID", got.QuestionID)
			}
		}
	}
	// The first exam keeps its original occurrence values.
	assertSemanticallyEqual(t, first, firstOut)
}

func TestFingerprintIsDeterministicAndVersioned(t *testing.T) {
	exam := loadGolden(t)
	for _, section := range exam.Sections {
		for _, q := range section.Questions {
			one, err := store.QuestionFingerprint(q)
			if err != nil {
				t.Fatal(err)
			}
			two, err := store.QuestionFingerprint(cloneQuestion(q))
			if err != nil || one != two {
				t.Errorf("fingerprint is not stable: %s vs %s (%v)", one, two, err)
			}
			if len(one) != 64 {
				t.Errorf("fingerprint %q is not a SHA-256 hex", one)
			}
		}
	}
	if store.FingerprintVersion != 1 {
		t.Errorf("store.FingerprintVersion = %d; a change needs a migration and backfill", store.FingerprintVersion)
	}
}

// questionBySourceID finds the stored row whose content_json keeps the
// source question_id.
func questionBySourceID(t *testing.T, db *storetest.MemDB, sourceID string) sqlcgen.Question {
	t.Helper()
	for _, q := range db.Questions() {
		var content ir.QuestionItem
		if err := json.Unmarshal(q.ContentJson, &content); err != nil {
			t.Fatal(err)
		}
		if content.QuestionID == sourceID {
			return q
		}
	}
	t.Fatalf("no stored question with source id %q", sourceID)
	return sqlcgen.Question{}
}

// cloneQuestion deep-copies a question through JSON.
func cloneQuestion(q ir.QuestionItem) ir.QuestionItem {
	data, err := json.Marshal(q)
	if err != nil {
		panic(err)
	}
	var out ir.QuestionItem
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func TestFingerprintExcludesOccurrenceAndUntrustedFields(t *testing.T) {
	q := firstQuestion(loadGolden(t))
	base, _ := store.QuestionFingerprint(q)

	same := []struct {
		name   string
		mutate func(*ir.QuestionItem)
	}{
		{"question_id", func(q *ir.QuestionItem) { q.QuestionID = "another-id" }},
		{"display_number", func(q *ir.QuestionItem) { q.DisplayNumber = "99" }},
		{"score", func(q *ir.QuestionItem) { q.Score = 42 }},
		{"metadata.dedup_fingerprint", func(q *ir.QuestionItem) {
			meta := map[string]any{}
			for k, v := range q.Metadata {
				meta[k] = v
			}
			meta["dedup_fingerprint"] = "attacker-controlled"
			q.Metadata = meta
		}},
		{"metadata.knowledge_points", func(q *ir.QuestionItem) {
			q.Metadata = withMeta(q.Metadata, "knowledge_points", []any{"函数", "导数"})
		}},
		{"metadata.difficulty", func(q *ir.QuestionItem) { q.Metadata = withMeta(q.Metadata, "difficulty", 0.9) }},
		{"provenance", func(q *ir.QuestionItem) { q.Provenance = map[string]any{"review_status": "pending"} }},
		{"child question_id", func(q *ir.QuestionItem) {
			if len(q.Children) > 0 {
				q.Children[0].QuestionID = "changed-child"
			}
		}},
	}
	for _, c := range same {
		mutated := cloneQuestion(q)
		c.mutate(&mutated)
		got, err := store.QuestionFingerprint(mutated)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != base {
			t.Errorf("changing %s changed the fingerprint", c.name)
		}
	}

	different := []struct {
		name   string
		mutate func(*ir.QuestionItem)
	}{
		{"stem text", func(q *ir.QuestionItem) { q.Stem[0].Markdown += "!" }},
		{"option label", func(q *ir.QuestionItem) { q.Options[0].Label = "E" }},
		{"answer", func(q *ir.QuestionItem) { q.Answer.ShortValues = []string{"C"} }},
		{"analysis", func(q *ir.QuestionItem) {
			q.Analysis = append(q.Analysis, ir.ContentBlock{Type: "text", Markdown: "more"})
		}},
		{"question_type", func(q *ir.QuestionItem) { q.QuestionType = "multiple_choice" }},
		{"other metadata", func(q *ir.QuestionItem) { q.Metadata = withMeta(q.Metadata, "source_tag", "x") }},
	}
	for _, c := range different {
		mutated := cloneQuestion(q)
		c.mutate(&mutated)
		got, err := store.QuestionFingerprint(mutated)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got == base {
			t.Errorf("changing %s did not change the fingerprint", c.name)
		}
	}
	// Nested children participate in the parent fingerprint.
	composite := lastQuestion(loadGolden(t))
	compositeFP, _ := store.QuestionFingerprint(composite)
	composite.Children[0].Stem[0].Markdown += "!"
	if got, _ := store.QuestionFingerprint(composite); got == compositeFP {
		t.Error("changing a child stem did not change the composite fingerprint")
	}
}

func withMeta(meta map[string]any, key string, value any) map[string]any {
	out := map[string]any{}
	for k, v := range meta {
		out[k] = v
	}
	out[key] = value
	return out
}

func firstQuestion(exam ir.ExamDocumentIR) ir.QuestionItem {
	return cloneQuestion(exam.Sections[0].Questions[0])
}

func lastQuestion(exam ir.ExamDocumentIR) ir.QuestionItem {
	last := exam.Sections[len(exam.Sections)-1]
	return cloneQuestion(last.Questions[len(last.Questions)-1])
}

func TestEqualFingerprintCanHaveSeveralEntities(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	persist(t, a, db, source, activeJob(t, db, sourceSHA))

	// Same semantics, different manual review state: never merged silently.
	other := clone(t, source)
	other.ExamID = "exam_with_pending_review"
	other.Sections[0].Questions[0].Provenance["review_status"] = "pending"
	persist(t, a, db, other, activeJob(t, db, sourceSHA))

	questions := db.Questions()
	if len(questions) != 4 {
		t.Fatalf("%d questions, want 4 (3 shared + 1 new entity for the revised question)", len(questions))
	}
	counts := map[string]int{}
	for _, q := range questions {
		counts[q.ContentFingerprint]++
	}
	shared := 0
	for _, n := range counts {
		if n > 1 {
			shared = n
		}
	}
	if shared != 2 {
		t.Errorf("no fingerprint is shared by two entities: %v", counts)
	}
	// The fingerprint index is not unique: two entities coexist.
	out, err := a.Assemble(t.Context(), db, recExamID(t, db, other.ExamID))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if got := out.Sections[0].Questions[0].Provenance["review_status"]; got != "pending" {
		t.Errorf("revised question assembled with review_status %v", got)
	}
}

func recExamID(t *testing.T, db *storetest.MemDB, irExamID string) uuid.UUID {
	t.Helper()
	for _, exam := range db.Exams() {
		if exam.IrExamID == irExamID {
			return uuid.UUID(exam.ExamID.Bytes)
		}
	}
	t.Fatalf("exam %q not found", irExamID)
	return uuid.Nil
}

func TestAssetConflictRules(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	id := "ast_01jexam0000000000000000001"
	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	before := db.Assets()[0]

	// Same ID + same SHA: idempotent, the row is unchanged.
	persist(t, a, db, clone(t, source), activeJob(t, db, sourceSHA))
	if got := db.Assets(); len(got) != 1 || !reflect.DeepEqual(got[0], before) {
		t.Errorf("idempotent persist changed the asset row: %+v", got)
	}

	// Same ID + different SHA: rejected, the registered row keeps its digest.
	conflict := store.SourceRecord{JobID: activeJob(t, db, sourceSHA), SourceSHA256: sourceSHA, Assets: []store.VerifiedAsset{
		{AssetID: id, StorageKey: before.StorageKey, SHA256: strings.Repeat("cd", 32)},
	}}
	_, err := a.Persist(t.Context(), db, clone(t, source), conflict)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different SHA err = %v, want store.ErrConflict", err)
	}
	if got := db.Assets()[0].Sha256; got != before.Sha256 {
		t.Errorf("asset digest was overwritten: %s", got)
	}

	// Same bytes but different identity metadata: rejected, not updated.
	changed := clone(t, source)
	stretched := changed.Assets[id]
	stretched.AspectRatio = 2
	changed.Assets[id] = stretched
	if _, err := a.Persist(t.Context(), db, changed, store.SourceRecord{JobID: activeJob(t, db, sourceSHA), SourceSHA256: sourceSHA, Assets: uploads(t, source)}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("changed geometry err = %v, want store.ErrConflict", err)
	}
	if got := db.Assets()[0]; got.MediaType != before.MediaType || got.AspectRatio != before.AspectRatio {
		t.Errorf("asset identity metadata was updated: %+v", got)
	}

	// Caption is display text: a non-empty new value is the allowed update.
	captioned := clone(t, source)
	relabelled := captioned.Assets[id]
	relabelled.Caption = "新的图注"
	captioned.Assets[id] = relabelled
	persist(t, a, db, captioned, activeJob(t, db, sourceSHA))
	if got := db.Assets()[0].Caption.String; got != "新的图注" {
		t.Errorf("caption = %q, want the updated text", got)
	}

}

// TestConcurrentAssetInsertIsIdempotent reproduces the read-then-insert race
// two simultaneous ingests of the same source hit: both read no asset row,
// both insert, and the loser gets ON CONFLICT DO NOTHING (no row returned).
// The loser must re-read the winner's row and accept it when the SHA-256 and
// identity match, instead of surfacing a unique-constraint conflict. That bug
// appeared only against a real PostgreSQL, so the race is modelled offline.
func TestConcurrentAssetInsertIsIdempotent(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	id := "ast_01jexam0000000000000000001"

	// The concurrent winner writes the same row this transaction is about to.
	winner := storetest.New()
	persist(t, a, winner, source, activeJob(t, winner, sourceSHA))
	db.RaceAsset(id, winner.Assets()[0])

	persist(t, a, db, source, activeJob(t, db, sourceSHA))
	if got := db.Assets(); len(got) != 1 || got[0].Sha256 != storage.HashBytes(mustRead(t, goldenAsset)) {
		t.Fatalf("after losing the race: assets = %+v, want the winner's single row", got)
	}

	// Losing the race to a row with a different digest is still a conflict.
	db2 := storetest.New()
	winner2 := storetest.New()
	persist(t, a, winner2, source, activeJob(t, winner2, sourceSHA))
	changed := winner2.Assets()[0]
	changed.Sha256 = strings.Repeat("cd", 32)
	db2.RaceAsset(id, changed)
	if _, err := a.Persist(t.Context(), db2, clone(t, source), store.SourceRecord{
		JobID: activeJob(t, db2, sourceSHA), SourceSHA256: sourceSHA, Assets: uploads(t, source),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("race with a different digest err = %v, want store.ErrConflict", err)
	}
}

// uploads returns verified upload records for the exam's assets, using the
// golden PNG bytes for each object.
func uploads(t *testing.T, exam ir.ExamDocumentIR) []store.VerifiedAsset {
	t.Helper()
	png := mustRead(t, goldenAsset)
	var out []store.VerifiedAsset
	for id, asset := range exam.Assets {
		key, err := storage.RawAssetKey(id, asset.MediaType)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, store.VerifiedAsset{AssetID: id, StorageKey: key, SHA256: storage.HashBytes(png)})
	}
	return out
}

func TestPersistRejectsInvalidInputAndWritesNothing(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)

	cases := map[string]func(*ir.ExamDocumentIR, *store.SourceRecord){
		"schema-invalid IR": func(exam *ir.ExamDocumentIR, _ *store.SourceRecord) {
			exam.Sections[0].Questions[0].Stem = append(exam.Sections[0].Questions[0].Stem, ir.ContentBlock{Type: "image_ref", AssetID: "ast_missing"})
		},
		"missing upload": func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.Assets = nil },
		"upload without IR asset": func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) {
			rec.Assets = append(rec.Assets, store.VerifiedAsset{AssetID: "ast_orphan000000000000000001", StorageKey: "assets/raw/ast_orphan000000000000000001.png", SHA256: strings.Repeat("ef", 32)})
		},
		"bad digest":        func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.Assets[0].SHA256 = "not-hex" },
		"non-canonical key": func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.Assets[0].StorageKey = "assets/raw/other.png" },
		"traversal key":     func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.Assets[0].StorageKey = "../../etc/passwd" },
		"no job":            func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.JobID = uuid.Nil },
		"bad source digest": func(_ *ir.ExamDocumentIR, rec *store.SourceRecord) { rec.SourceSHA256 = "" },
		"empty subject":     func(exam *ir.ExamDocumentIR, _ *store.SourceRecord) { exam.Metadata.Subject = "" },
	}
	for name, mutate := range cases {
		exam := clone(t, source)
		rec := store.SourceRecord{JobID: activeJob(t, db, sourceSHA), SourceSHA256: sourceSHA, Assets: uploads(t, source)}
		mutate(&exam, &rec)
		if _, err := a.Persist(t.Context(), db, exam, rec); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: err = %v, want store.ErrInvalid", name, err)
		}
	}
	if len(db.Assets())+len(db.Questions())+len(db.Exams()) != 0 {
		t.Errorf("a rejected Persist wrote rows: %d assets, %d questions, %d exams",
			len(db.Assets()), len(db.Questions()), len(db.Exams()))
	}
}

func TestPersistNeedsAValidatingJobAndRollsBack(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	jobID := uuid.New()
	job := pgtype.UUID{Bytes: jobID, Valid: true}
	if _, err := db.InsertIngestJob(t.Context(), sqlcgen.InsertIngestJobParams{JobID: job, SourcePdfKey: "uploads/" + jobID.String() + "/source.pdf", SourceFileSha256: sourceSHA}); err != nil {
		t.Fatal(err)
	}
	rec := store.SourceRecord{JobID: jobID, SourceSHA256: sourceSHA, Assets: uploads(t, source)}

	// A queued job cannot be completed: the final conditional update fails.
	err := db.InTx(t.Context(), func(ctx context.Context) error {
		_, err := a.Persist(ctx, db, clone(t, source), rec)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Persist with a queued job err = %v, want store.ErrNotFound", err)
	}
	// Everything written before the failing step is gone.
	if len(db.Assets())+len(db.Questions())+len(db.Exams())+db.CountItems() != 0 {
		t.Error("rows survived a rolled-back Persist")
	}
	if job, _ := db.GetIngestJob(t.Context(), job); job.State != store.JobQueued {
		t.Errorf("job state = %q after rollback", job.State)
	}
}

func TestAssembleRejectsCorruptRows(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	source := loadGolden(t)
	rec := persist(t, a, db, source, activeJob(t, db, sourceSHA))
	if _, err := a.Assemble(t.Context(), db, rec.ExamID); err != nil {
		t.Fatalf("baseline Assemble: %v", err)
	}

	cases := map[string]func(*storetest.MemDB){
		"unknown question field": func(db *storetest.MemDB) {
			db.MutateQuestions(func(q *sqlcgen.Question) {
				q.ContentJson = []byte(`{"kind":"single","display_number":"1","stem":[],"future_field":1}`)
			})
		},
		"wrong content type": func(db *storetest.MemDB) {
			db.MutateQuestions(func(q *sqlcgen.Question) { q.ContentJson = []byte(`{"kind":"single","display_number":1,"stem":[]}`) })
		},
		"missing asset":       func(db *storetest.MemDB) { db.DeleteAssets("ast_01jexam0000000000000000001") },
		"missing question":    func(db *storetest.MemDB) { db.DeleteQuestions() },
		"broken item ordinal": func(db *storetest.MemDB) { db.MutateItems(func(item *sqlcgen.ExamItem) { item.Ordinal = 7 }) },
		"corrupt exam metadata": func(db *storetest.MemDB) {
			db.MutateExams(func(e *sqlcgen.Exam) { e.MetadataJson = []byte(`{"title":42}`) })
		},
		"projection disagreement": func(db *storetest.MemDB) {
			db.MutateQuestions(func(q *sqlcgen.Question) { q.Kind = "composite" })
		},
	}
	for name, corrupt := range cases {
		snapshot := db.Clone()
		corrupt(db)
		exam, err := a.Assemble(t.Context(), db, rec.ExamID)
		if err == nil || (!errors.Is(err, store.ErrInvalid) && !errors.Is(err, store.ErrNotFound)) {
			t.Errorf("%s: err = %v, want store.ErrInvalid or store.ErrNotFound", name, err)
		}
		if exam.SchemaVersion != "" || len(exam.Sections) != 0 {
			t.Errorf("%s: a partial document was returned: %+v", name, exam)
		}
		db.Restore(snapshot)
	}
	if _, err := a.Assemble(t.Context(), db, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown exam err = %v, want store.ErrNotFound", err)
	}
}
