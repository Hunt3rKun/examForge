// Package storetest provides an in-memory Workspace for offline tests. It
// mirrors the schema's keys and references with plain Go maps; it does not
// execute SQL. Integration tests against a real PostgreSQL are the authority
// for the queries themselves.
package storetest

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
)

// MemDB is an in-memory Workspace with the schema's unique keys and
// references. It is safe for concurrent use.
type MemDB struct {
	mu        sync.Mutex
	assets    map[string]sqlcgen.Asset
	questions map[pgtype.UUID]sqlcgen.Question
	exams     map[pgtype.UUID]sqlcgen.Exam
	sections  map[pgtype.UUID]sqlcgen.ExamSection
	items     map[pgtype.UUID]sqlcgen.ExamItem
	jobs      map[pgtype.UUID]sqlcgen.IngestJob

	// FailNext makes the next n mutating calls fail with FailErr. A test sets
	// it to model a database outage in the middle of a transaction.
	FailNext int
	FailErr  error

	// assetRaces maps an asset ID to the row a concurrent transaction
	// "creates" just before this one's first insert, modelling the read-then-
	// insert race two ingests of equal content hit in production. The insert
	// then hits the real conflict path (no row returned) instead of the
	// already-exists path, so the conflict handling is exercised offline.
	assetRaces map[string]sqlcgen.Asset
}

// RaceAsset makes the next InsertAsset for assetID behave as if another
// transaction inserted row first.
func (m *MemDB) RaceAsset(assetID string, row sqlcgen.Asset) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.assetRaces == nil {
		m.assetRaces = map[string]sqlcgen.Asset{}
	}
	m.assetRaces[assetID] = row
}

var (
	_ store.Workspace  = (*MemDB)(nil)
	_ store.IngestJobs = (*MemDB)(nil)
)

// New returns an empty store.
func New() *MemDB {
	return &MemDB{
		assets:    map[string]sqlcgen.Asset{},
		questions: map[pgtype.UUID]sqlcgen.Question{},
		exams:     map[pgtype.UUID]sqlcgen.Exam{},
		sections:  map[pgtype.UUID]sqlcgen.ExamSection{},
		items:     map[pgtype.UUID]sqlcgen.ExamItem{},
		jobs:      map[pgtype.UUID]sqlcgen.IngestJob{},
	}
}

// InTx runs fn against the store and restores the previous contents when fn
// fails or panics. It models the rollback of a real transaction.
func (m *MemDB) InTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	snapshot := m.Clone()
	defer func() {
		if p := recover(); p != nil {
			m.Restore(snapshot)
			panic(p)
		}
	}()
	if err = fn(ctx); err != nil {
		m.Restore(snapshot)
	}
	return err
}

// Clone returns an independent copy; Restore puts it back.
func (m *MemDB) Clone() *MemDB {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &MemDB{
		assets:    cloneMap(m.assets),
		questions: cloneMap(m.questions),
		exams:     cloneMap(m.exams),
		sections:  cloneMap(m.sections),
		items:     cloneMap(m.items),
		jobs:      cloneMap(m.jobs),
	}
}

// Restore replaces the contents with those of the snapshot.
func (m *MemDB) Restore(snapshot *MemDB) {
	fresh := snapshot.Clone()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assets, m.questions, m.exams, m.sections, m.items, m.jobs =
		fresh.assets, fresh.questions, fresh.exams, fresh.sections, fresh.items, fresh.jobs
}

func cloneMap[K comparable, V any](src map[K]V) map[K]V {
	out := make(map[K]V, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Must be called with mu held.
func (m *MemDB) fail() error {
	if m.FailNext <= 0 {
		return nil
	}
	m.FailNext--
	return m.FailErr
}

// Snapshot accessors return copies in a stable order for assertions.
func (m *MemDB) Assets() []sqlcgen.Asset {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sqlcgen.Asset, 0, len(m.assets))
	for key := range m.assets {
		out = append(out, m.assets[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out
}

func (m *MemDB) Questions() []sqlcgen.Question {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sqlcgen.Question, 0, len(m.questions))
	for id := range m.questions {
		out = append(out, m.questions[id])
	}
	sort.Slice(out, func(i, j int) bool { return uuidString(out[i].QuestionID) < uuidString(out[j].QuestionID) })
	return out
}

func (m *MemDB) Exams() []sqlcgen.Exam {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sqlcgen.Exam, 0, len(m.exams))
	for id := range m.exams {
		out = append(out, m.exams[id])
	}
	sort.Slice(out, func(i, j int) bool { return uuidString(out[i].ExamID) < uuidString(out[j].ExamID) })
	return out
}

func (m *MemDB) JobRows() []sqlcgen.IngestJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sqlcgen.IngestJob, 0, len(m.jobs))
	for id := range m.jobs {
		out = append(out, m.jobs[id])
	}
	sort.Slice(out, func(i, j int) bool { return uuidString(out[i].JobID) < uuidString(out[j].JobID) })
	return out
}

// CountItems returns the number of exam_items rows.
func (m *MemDB) CountItems() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// ── assets ──────────────────────────────────────────────────────────────────

func (m *MemDB) GetAsset(_ context.Context, assetID string) (sqlcgen.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.assets[assetID]
	if !ok {
		return sqlcgen.Asset{}, store.ErrNotFound
	}
	return row, nil
}

func (m *MemDB) InsertAsset(_ context.Context, arg sqlcgen.InsertAssetParams) (sqlcgen.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.Asset{}, err
	}
	// A pending race installs the concurrent winner's row first, exactly as a
	// committed transaction would have before this INSERT ran.
	if winner, ok := m.assetRaces[arg.AssetID]; ok {
		delete(m.assetRaces, arg.AssetID)
		if _, exists := m.assets[arg.AssetID]; !exists {
			m.assets[arg.AssetID] = winner
		}
	}
	if _, exists := m.assets[arg.AssetID]; exists {
		// Mirrors `ON CONFLICT (asset_id) DO NOTHING`: no row is returned, so
		// the caller re-reads and applies the SHA-256/metadata rules.
		return sqlcgen.Asset{}, store.ErrNotFound
	}
	for _, row := range m.assets {
		if row.StorageKey == arg.StorageKey {
			return sqlcgen.Asset{}, conflict("assets_storage_key_key")
		}
	}
	row := sqlcgen.Asset{
		AssetID: arg.AssetID, MediaType: arg.MediaType, StorageKey: arg.StorageKey, RawStorageKey: arg.RawStorageKey,
		Status: arg.Status, AspectRatio: arg.AspectRatio, SuggestedWidthMm: arg.SuggestedWidthMm,
		Caption: arg.Caption, Alt: arg.Alt, SourceBbox: append([]byte(nil), arg.SourceBbox...), Sha256: arg.Sha256,
	}
	m.assets[arg.AssetID] = row
	return row, nil
}

func (m *MemDB) UpdateAssetDisplay(_ context.Context, arg sqlcgen.UpdateAssetDisplayParams) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return 0, err
	}
	row, ok := m.assets[arg.AssetID]
	if !ok {
		return 0, nil
	}
	if arg.Caption != "" {
		row.Caption = pgtype.Text{String: arg.Caption, Valid: true}
	}
	if arg.Alt != "" {
		row.Alt = pgtype.Text{String: arg.Alt, Valid: true}
	}
	m.assets[arg.AssetID] = row
	return 1, nil
}

func (m *MemDB) ListAssets(_ context.Context, assetIDs []string) ([]sqlcgen.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sqlcgen.Asset
	for _, id := range sorted(assetIDs) {
		if row, ok := m.assets[id]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// ── questions ────────────────────────────────────────────────────────────────

func (m *MemDB) GetQuestion(_ context.Context, id pgtype.UUID) (sqlcgen.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.questions[id]
	if !ok {
		return sqlcgen.Question{}, store.ErrNotFound
	}
	return row, nil
}

func (m *MemDB) ListQuestions(_ context.Context, questionIDs []pgtype.UUID) ([]sqlcgen.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sqlcgen.Question
	for _, id := range questionIDs {
		if row, ok := m.questions[id]; ok && !containsQuestion(out, row) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (m *MemDB) ListQuestionsByFingerprint(_ context.Context, arg sqlcgen.ListQuestionsByFingerprintParams) ([]sqlcgen.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sqlcgen.Question
	for id, row := range m.questions {
		if row.ContentFingerprint == arg.ContentFingerprint && row.FingerprintVersion == arg.FingerprintVersion {
			out = append(out, m.questions[id])
		}
	}
	sort.Slice(out, func(i, j int) bool { return uuidString(out[i].QuestionID) < uuidString(out[j].QuestionID) })
	return out, nil
}

func (m *MemDB) InsertQuestion(_ context.Context, arg sqlcgen.InsertQuestionParams) (sqlcgen.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.Question{}, err
	}
	// Mirror the column's NOT NULL constraint. pgx encodes a nil []string as
	// SQL NULL; PostgreSQL rejects it, and so does this fake, so the offline
	// gate catches a regression before it reaches a real database.
	if arg.KnowledgePoints == nil {
		return sqlcgen.Question{}, fmt.Errorf("%w: knowledge_points is null", store.ErrInvalid)
	}
	row := sqlcgen.Question{
		QuestionID: newID(), Subject: arg.Subject, GradeLevel: arg.GradeLevel, Kind: arg.Kind,
		QuestionType: arg.QuestionType, KnowledgePoints: arg.KnowledgePoints, Difficulty: arg.Difficulty,
		ContentFingerprint: arg.ContentFingerprint, FingerprintVersion: arg.FingerprintVersion,
		ContentJson: append([]byte(nil), arg.ContentJson...),
	}
	m.questions[row.QuestionID] = row
	return row, nil
}

// ── exams ───────────────────────────────────────────────────────────────────

func (m *MemDB) GetExam(_ context.Context, id pgtype.UUID) (sqlcgen.Exam, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.exams[id]
	if !ok {
		return sqlcgen.Exam{}, store.ErrNotFound
	}
	return row, nil
}

func (m *MemDB) GetExamByJob(_ context.Context, jobID pgtype.UUID) (sqlcgen.Exam, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, row := range m.exams {
		if row.IngestJobID == jobID {
			return m.exams[id], nil
		}
	}
	return sqlcgen.Exam{}, store.ErrNotFound
}

func (m *MemDB) InsertExam(_ context.Context, arg sqlcgen.InsertExamParams) (sqlcgen.Exam, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.Exam{}, err
	}
	if _, exists := m.jobs[arg.IngestJobID]; !exists {
		return sqlcgen.Exam{}, conflict("exams_ingest_job_id_fkey")
	}
	for _, row := range m.exams {
		if row.IngestJobID == arg.IngestJobID {
			return sqlcgen.Exam{}, conflict("exams_ingest_job_id_key")
		}
	}
	row := sqlcgen.Exam{
		ExamID: newID(), IrExamID: arg.IrExamID, IngestJobID: arg.IngestJobID,
		SourceFileSha256: arg.SourceFileSha256, SourcePdfKey: arg.SourcePdfKey,
		SchemaVersion: arg.SchemaVersion, MetadataJson: append([]byte(nil), arg.MetadataJson...),
	}
	m.exams[row.ExamID] = row
	return row, nil
}

func (m *MemDB) UpdateExam(_ context.Context, arg sqlcgen.UpdateExamParams) (sqlcgen.Exam, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.Exam{}, err
	}
	row, ok := m.exams[arg.ExamID]
	if !ok {
		return sqlcgen.Exam{}, store.ErrNotFound
	}
	row.IrExamID, row.SourceFileSha256, row.SourcePdfKey = arg.IrExamID, arg.SourceFileSha256, arg.SourcePdfKey
	row.SchemaVersion, row.MetadataJson = arg.SchemaVersion, append([]byte(nil), arg.MetadataJson...)
	m.exams[arg.ExamID] = row
	return row, nil
}

// DeleteSections removes a section and cascades to its items.
func (m *MemDB) DeleteSections(_ context.Context, examID pgtype.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return 0, err
	}
	var removed int64
	for id, sec := range m.sections {
		if sec.ExamID != examID {
			continue
		}
		delete(m.sections, id)
		for itemID, item := range m.items {
			if item.SectionID == id {
				delete(m.items, itemID)
				removed++
			}
		}
	}
	return removed, nil
}

func (m *MemDB) InsertSection(_ context.Context, arg sqlcgen.InsertSectionParams) (sqlcgen.ExamSection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.ExamSection{}, err
	}
	if _, exists := m.exams[arg.ExamID]; !exists {
		return sqlcgen.ExamSection{}, conflict("exam_sections_exam_id_fkey")
	}
	for _, sec := range m.sections {
		if sec.ExamID == arg.ExamID && (sec.Ordinal == arg.Ordinal || sec.IrSectionID == arg.IrSectionID) {
			return sqlcgen.ExamSection{}, conflict("exam_sections unique")
		}
	}
	row := sqlcgen.ExamSection{
		SectionID: newID(), ExamID: arg.ExamID, IrSectionID: arg.IrSectionID, Ordinal: arg.Ordinal,
		Title: arg.Title, Instruction: arg.Instruction,
	}
	m.sections[row.SectionID] = row
	return row, nil
}

func (m *MemDB) InsertItem(_ context.Context, arg sqlcgen.InsertItemParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return err
	}
	if _, exists := m.sections[arg.SectionID]; !exists {
		return conflict("exam_items_section_id_fkey")
	}
	if _, exists := m.questions[arg.QuestionID]; !exists {
		return conflict("exam_items_question_id_fkey")
	}
	for _, item := range m.items {
		if item.SectionID == arg.SectionID && item.Ordinal == arg.Ordinal {
			return conflict("exam_items unique")
		}
	}
	m.items[newID()] = sqlcgen.ExamItem{
		SectionID: arg.SectionID, QuestionID: arg.QuestionID, Ordinal: arg.Ordinal,
		DisplayNumberOverride: arg.DisplayNumberOverride, ScoreOverride: arg.ScoreOverride,
	}
	return nil
}

func (m *MemDB) ListSections(_ context.Context, examID pgtype.UUID) ([]sqlcgen.ExamSection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sqlcgen.ExamSection
	for _, sec := range m.sections {
		if sec.ExamID == examID {
			out = append(out, sec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

func (m *MemDB) ListItems(_ context.Context, sectionID pgtype.UUID) ([]sqlcgen.ExamItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sqlcgen.ExamItem
	for _, item := range m.items {
		if item.SectionID == sectionID {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

// ── ingest jobs (compare-and-set on state) ───────────────────────────────────

func (m *MemDB) InsertIngestJob(_ context.Context, arg sqlcgen.InsertIngestJobParams) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.IngestJob{}, err
	}
	if _, exists := m.jobs[arg.JobID]; exists {
		return sqlcgen.IngestJob{}, conflict("ingest_jobs_pkey")
	}
	row := sqlcgen.IngestJob{
		JobID: arg.JobID, SourcePdfKey: arg.SourcePdfKey, SourceFileSha256: arg.SourceFileSha256, State: store.JobQueued,
	}
	m.jobs[arg.JobID] = row
	return row, nil
}

func (m *MemDB) SetRiverJobID(_ context.Context, arg sqlcgen.SetRiverJobIDParams) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return 0, err
	}
	row, ok := m.jobs[arg.JobID]
	if !ok {
		return 0, nil
	}
	for id, other := range m.jobs {
		if id != arg.JobID && other.RiverJobID.Valid && other.RiverJobID.String == arg.RiverJobID.String {
			return 0, conflict("ingest_jobs_river_job_id_key")
		}
	}
	row.RiverJobID = arg.RiverJobID
	m.jobs[arg.JobID] = row
	return 1, nil
}

func (m *MemDB) GetIngestJob(_ context.Context, jobID pgtype.UUID) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.jobs[jobID]
	if !ok {
		return sqlcgen.IngestJob{}, store.ErrNotFound
	}
	return row, nil
}

// ClaimIngestJob is the conditional update of a worker start. As in SQL, a
// job whose state is not in the expected set updates no row: ErrNotFound.
func (m *MemDB) ClaimIngestJob(_ context.Context, arg sqlcgen.ClaimIngestJobParams) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row, err := m.moveLocked(arg.JobID, arg.States, store.JobParsing)
	if err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row.AttemptCount++
	m.jobs[arg.JobID] = row
	return row, nil
}

func (m *MemDB) MoveIngestJob(_ context.Context, arg sqlcgen.MoveIngestJobParams) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.IngestJob{}, err
	}
	return m.moveLocked(arg.JobID, arg.FromStates, arg.State)
}

func (m *MemDB) FailIngestJob(_ context.Context, arg sqlcgen.FailIngestJobParams) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row, err := m.moveLocked(arg.JobID, arg.FromStates, store.JobFailed)
	if err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row.ErrorCode, row.ErrorMessage = arg.ErrorCode, arg.ErrorMessage
	m.jobs[arg.JobID] = row
	return row, nil
}

func (m *MemDB) CompleteIngestJob(_ context.Context, arg sqlcgen.CompleteIngestJobParams) (sqlcgen.IngestJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(); err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row, err := m.moveLocked(arg.JobID, []string{store.JobValidating}, store.JobPersisted)
	if err != nil {
		return sqlcgen.IngestJob{}, err
	}
	row.ExamID, row.ErrorCode, row.ErrorMessage = arg.ExamID, pgtype.Text{}, pgtype.Text{}
	m.jobs[arg.JobID] = row
	return row, nil
}

func (m *MemDB) DeleteIngestJob(_ context.Context, jobID pgtype.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; !ok {
		return 0, nil
	}
	delete(m.jobs, jobID)
	return 1, nil
}

// moveLocked changes the state only when the current state is in from. A
// missing row or an unexpected state yields ErrNotFound, exactly like an
// UPDATE ... WHERE that matches no row.
func (m *MemDB) moveLocked(jobID pgtype.UUID, from []string, to string) (sqlcgen.IngestJob, error) {
	row, ok := m.jobs[jobID]
	if !ok || !contains(from, row.State) {
		return sqlcgen.IngestJob{}, store.ErrNotFound
	}
	row.State = to
	m.jobs[jobID] = row
	return row, nil
}

// ── direct row access for corrupting state in tests ────────────────────────

func (m *MemDB) MutateQuestions(fn func(q *sqlcgen.Question)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, row := range m.questions {
		fn(&row)
		m.questions[id] = row
	}
}

func (m *MemDB) MutateExams(fn func(e *sqlcgen.Exam)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, row := range m.exams {
		fn(&row)
		m.exams[id] = row
	}
}

func (m *MemDB) MutateItems(fn func(item *sqlcgen.ExamItem)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, row := range m.items {
		fn(&row)
		m.items[id] = row
	}
}

// DeleteAssets removes asset rows, ignoring unknown IDs.
func (m *MemDB) DeleteAssets(ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.assets, id)
	}
}

// DeleteQuestions removes every question row.
func (m *MemDB) DeleteQuestions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.questions = map[pgtype.UUID]sqlcgen.Question{}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newID() pgtype.UUID {
	var id uuid.UUID = uuid.New()
	return pgtype.UUID{Bytes: id, Valid: true}
}

func uuidString(id pgtype.UUID) string {
	return uuid.UUID(id.Bytes).String()
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func containsQuestion(list []sqlcgen.Question, row sqlcgen.Question) bool {
	for _, item := range list {
		if item.QuestionID == row.QuestionID {
			return true
		}
	}
	return false
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func conflict(constraint string) error {
	return fmt.Errorf("%w: unique constraint %s", store.ErrConflict, constraint)
}

// Persister is the in-memory counterpart of store.Persister: it runs the
// assembler inside MemDB.InTx so a failure rolls back every row.
type Persister struct {
	db        *MemDB
	assembler *store.Assembler
}

// NewPersister binds the assembler to the in-memory store.
func NewPersister(db *MemDB, assembler *store.Assembler) (*Persister, error) {
	if db == nil || assembler == nil {
		return nil, store.ErrInvalid
	}
	return &Persister{db: db, assembler: assembler}, nil
}

// Persist validates and writes the exam in one simulated transaction.
func (p *Persister) Persist(ctx context.Context, exam ir.ExamDocumentIR, source store.SourceRecord) (rec store.ExamRecord, err error) {
	err = p.db.InTx(ctx, func(ctx context.Context) error {
		var err error
		rec, err = p.assembler.Persist(ctx, p.db, exam, source)
		return err
	})
	return rec, err
}

// AssetRecord implements storage.AssetLookup over the in-memory rows.
func (m *MemDB) AssetRecord(ctx context.Context, assetID string) (storage.AssetRecord, error) {
	row, err := m.GetAsset(ctx, assetID)
	if err != nil {
		return storage.AssetRecord{}, err
	}
	return storage.AssetRecord{AssetID: row.AssetID, StorageKey: row.StorageKey, MediaType: row.MediaType, SHA256: row.Sha256}, nil
}
