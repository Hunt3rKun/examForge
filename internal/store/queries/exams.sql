-- name: GetExam :one
SELECT * FROM exams WHERE exam_id = $1;

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

-- name: InsertItem :exec
INSERT INTO exam_items (section_id, question_id, ordinal, display_number_override, score_override)
VALUES ($1, $2, $3, $4, $5);

-- name: ListSections :many
SELECT * FROM exam_sections WHERE exam_id = $1 ORDER BY ordinal;

-- name: ListItems :many
SELECT * FROM exam_items WHERE section_id = $1 ORDER BY ordinal;
