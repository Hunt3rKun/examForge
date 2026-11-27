-- name: GetQuestion :one
SELECT * FROM questions WHERE question_id = $1;

-- Candidates only. Several entities may share a fingerprint.
-- name: ListQuestionsByFingerprint :many
SELECT * FROM questions
WHERE content_fingerprint = $1 AND fingerprint_version = $2
ORDER BY question_id;

-- name: InsertQuestion :one
INSERT INTO questions (
    subject, grade_level, kind, question_type, knowledge_points, difficulty,
    content_fingerprint, fingerprint_version, content_json
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListQuestions :many
SELECT * FROM questions WHERE question_id = ANY(sqlc.arg(question_ids)::uuid[]) ORDER BY question_id;
