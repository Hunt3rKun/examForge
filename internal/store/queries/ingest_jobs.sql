-- Written in the same transaction as the River insert (river_job_id after).
-- name: InsertIngestJob :one
INSERT INTO ingest_jobs (job_id, source_pdf_key, source_file_sha256, state)
VALUES ($1, $2, $3, 'queued')
RETURNING *;

-- name: SetRiverJobID :execrows
UPDATE ingest_jobs SET river_job_id = $2, updated_at = now() WHERE job_id = $1;

-- name: GetIngestJob :one
SELECT * FROM ingest_jobs WHERE job_id = $1;

-- A worker start: queued first run, or re-entry into the active state after
-- an attempt that died without recording an outcome.
-- name: ClaimIngestJob :one
UPDATE ingest_jobs
SET state = 'parsing', attempt_count = attempt_count + 1,
    started_at = now(), error_code = NULL, error_message = NULL, updated_at = now()
WHERE job_id = $1 AND state = ANY(sqlc.arg(states)::text[])
RETURNING *;

-- Every later transition is a compare-and-set on the current state.
-- name: MoveIngestJob :one
UPDATE ingest_jobs
SET state = $2, updated_at = now()
WHERE job_id = $1 AND state = ANY(sqlc.arg(from_states)::text[])
RETURNING *;

-- name: FailIngestJob :one
UPDATE ingest_jobs
SET state = 'failed', error_code = $2, error_message = $3,
    finished_at = now(), updated_at = now()
WHERE job_id = $1 AND state = ANY(sqlc.arg(from_states)::text[])
RETURNING *;

-- Runs in the persistence transaction so the exam link commits with the exam.
-- name: CompleteIngestJob :one
UPDATE ingest_jobs
SET state = 'persisted', exam_id = $2, error_code = NULL, error_message = NULL,
    finished_at = now(), updated_at = now()
WHERE job_id = $1 AND state = 'validating'
RETURNING *;

-- name: DeleteIngestJob :execrows
DELETE FROM ingest_jobs WHERE job_id = $1;
