-- name: GetExam :one
SELECT * FROM exams WHERE exam_id = $1;

-- name: GetExamForUpdate :one
SELECT * FROM exams WHERE exam_id = $1 FOR UPDATE;

-- name: GetExamByJob :one
SELECT * FROM exams WHERE ingest_job_id = $1;

-- name: InsertExam :one
INSERT INTO exams (
    ingest_job_id, ir_exam_id, source_file_sha256, source_pdf_key, schema_version, metadata_json
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- Replaying a job refreshes its existing exam instead of adding another.
-- name: UpdateExam :one
UPDATE exams
SET ir_exam_id = $2, source_file_sha256 = $3, source_pdf_key = $4,
    schema_version = $5, metadata_json = $6, updated_at = now()
WHERE exam_id = $1
RETURNING *;

-- exam_items cascade from their section.
-- name: DeleteSections :execrows
DELETE FROM exam_sections WHERE exam_id = $1;

-- name: InsertSection :one
INSERT INTO exam_sections (exam_id, ir_section_id, ordinal, title, instruction)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- answer_space_mm is an occurrence value: it belongs to this item, never to
-- the shared question entity.
-- name: InsertItem :exec
INSERT INTO exam_items (
    section_id, question_id, ordinal, display_number_override, score_override,
    answer_space_mm_override
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListSections :many
SELECT * FROM exam_sections WHERE exam_id = $1 ORDER BY ordinal;

-- name: ListItems :many
SELECT * FROM exam_items WHERE section_id = $1 ORDER BY ordinal;

-- Every occurrence of one exam in IR order (section ordinal, then item
-- ordinal). Used by the API detail view and by the edit transaction.
-- name: ListExamItems :many
SELECT i.*
FROM exam_items i
JOIN exam_sections s ON s.section_id = i.section_id
WHERE s.exam_id = $1
ORDER BY s.ordinal, i.ordinal;

-- Locks one occurrence for an edit. The row must belong to the exam on the
-- path; the caller verifies that under the same lock.
-- name: GetExamItemForUpdate :one
SELECT i.*, s.exam_id
FROM exam_items i
JOIN exam_sections s ON s.section_id = i.section_id
WHERE i.item_id = $1
FOR UPDATE OF i;

-- Reference count of one question entity. Callers hold the question row lock
-- (FOR UPDATE) while counting, so a concurrent attach of the same question
-- cannot slip between the count and the rewrite.
-- name: CountQuestionReferences :one
SELECT count(*) FROM exam_items WHERE question_id = $1;

-- name: UpdateItemOccurrence :one
UPDATE exam_items
SET display_number_override = $2, score_override = $3,
    answer_space_mm_override = $4, revision = revision + 1
WHERE item_id = $1 AND revision = $5
RETURNING *;

-- One edit statement: entity relink (when the shared entity was cloned) and
-- occurrence values change together, and the revision compare-and-set makes a
-- stale If-Match fail instead of silently winning.
-- name: UpdateItem :one
UPDATE exam_items
SET question_id = $2, display_number_override = $3, score_override = $4,
    answer_space_mm_override = $5, revision = revision + 1
WHERE item_id = $1 AND revision = $6
RETURNING *;

-- Stable keyset pagination: created_at DESC, exam_id DESC. A NULL cursor is
-- encoded as infinity, so the first page starts at the newest exam.
-- Reference counts for one exam's questions, so a detail view can say which
-- items point at a shared entity.
-- name: ListQuestionReferenceCounts :many
SELECT question_id, count(*) AS references
FROM exam_items
WHERE question_id = ANY(sqlc.arg(question_ids)::uuid[])
GROUP BY question_id;

-- name: ListExamsPage :many
SELECT * FROM exams
WHERE (created_at, exam_id) < (
    COALESCE(sqlc.narg('cursor_created_at')::timestamptz, 'infinity'::timestamptz),
    COALESCE(sqlc.narg('cursor_exam_id')::uuid, 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid)
)
ORDER BY created_at DESC, exam_id DESC
LIMIT sqlc.arg('page_limit');
