-- name: GetQuestion :one
SELECT * FROM questions WHERE question_id = $1;

-- name: GetQuestionForUpdate :one
SELECT * FROM questions WHERE question_id = $1 FOR UPDATE;

-- Candidates only. Several entities may share a fingerprint.
-- name: ListQuestionsByFingerprint :many
SELECT * FROM questions
WHERE content_fingerprint = $1 AND fingerprint_version = $2
ORDER BY question_id;

-- The same candidate lookup under a row lock, in question_id order. Every
-- path that adds an exam_items reference takes this lock first, and the edit
-- path takes it before counting references, so the count cannot miss a
-- concurrent attach. The fixed order prevents deadlocks between two writers.
-- name: ListQuestionsByFingerprintForUpdate :many
SELECT * FROM questions
WHERE content_fingerprint = $1 AND fingerprint_version = $2
ORDER BY question_id
FOR UPDATE;

-- Row locks for the reference protocol. Every path that adds or moves an
-- exam_items reference takes these locks in sorted question_id order before
-- touching the reference, so a reference count and a relink can never
-- interleave. New rows inserted by the same transaction are already locked.
-- name: LockQuestions :many
SELECT question_id FROM questions
WHERE question_id = ANY(sqlc.arg(question_ids)::uuid[])
ORDER BY question_id
FOR UPDATE;

-- name: InsertQuestion :one
INSERT INTO questions (
    subject, grade_level, kind, question_type, knowledge_points, difficulty,
    content_fingerprint, fingerprint_version, content_json
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateQuestion :one
UPDATE questions
SET subject = $2, grade_level = $3, kind = $4, question_type = $5,
    knowledge_points = $6, difficulty = $7, content_fingerprint = $8,
    fingerprint_version = $9, content_json = $10, updated_at = now()
WHERE question_id = $1
RETURNING *;

-- Removes an entity the edit path orphaned. The NOT EXISTS guard makes the
-- delete safe even if another item attached in the meantime.
-- name: DeleteQuestionIfUnreferenced :execrows
DELETE FROM questions q
WHERE q.question_id = $1
  AND NOT EXISTS (SELECT 1 FROM exam_items i WHERE i.question_id = q.question_id);

-- name: ListQuestions :many
SELECT * FROM questions WHERE question_id = ANY(sqlc.arg(question_ids)::uuid[]) ORDER BY question_id;
