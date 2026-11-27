-- Ingest job bookkeeping. River owns its own job tables; this table only
-- mirrors the domain state and points at the River job by ID.
-- exam_id's foreign key is added in migration 0004, after exams exists.

CREATE TABLE ingest_jobs (
    job_id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_pdf_key     text NOT NULL,
    source_file_sha256 char(64) NOT NULL CHECK (source_file_sha256 ~ '^[0-9a-f]{64}$'),
    exam_id            uuid,
    river_job_id       text UNIQUE,
    state              text NOT NULL
        CHECK (state IN ('queued', 'parsing', 'validating', 'persisted', 'failed')),
    attempt_count      integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    error_code         text,
    error_message      text CHECK (error_message IS NULL OR char_length(error_message) <= 2000),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    finished_at        timestamptz
);

CREATE INDEX ingest_jobs_state_created_idx ON ingest_jobs (state, created_at);
-- Non-unique: different jobs may parse the same source PDF.
CREATE INDEX ingest_jobs_source_sha_idx ON ingest_jobs (source_file_sha256);
