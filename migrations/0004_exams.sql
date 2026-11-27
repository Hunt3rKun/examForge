-- Exams, their section/item orchestration, and the job-to-exam link.
-- questions and assets are shared and are never deleted through an exam.

CREATE TABLE exams (
    exam_id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ir_exam_id         text NOT NULL,
    ingest_job_id     uuid NOT NULL UNIQUE REFERENCES ingest_jobs (job_id),
    source_file_sha256 char(64) NOT NULL CHECK (source_file_sha256 ~ '^[0-9a-f]{64}$'),
    source_pdf_key    text NOT NULL,
    schema_version     text NOT NULL,
    metadata_json      jsonb NOT NULL CHECK (jsonb_typeof(metadata_json) = 'object'),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Non-unique: equal source content may be parsed by several jobs.
CREATE INDEX exams_source_sha_idx ON exams (source_file_sha256);
CREATE INDEX exams_ir_exam_id_idx ON exams (ir_exam_id);

CREATE TABLE exam_sections (
    section_id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    exam_id      uuid NOT NULL REFERENCES exams (exam_id) ON DELETE CASCADE,
    ir_section_id text NOT NULL,
    ordinal      integer NOT NULL CHECK (ordinal >= 0),
    title        text NOT NULL,
    instruction  text,
    UNIQUE (exam_id, ordinal),
    UNIQUE (exam_id, ir_section_id)
);

CREATE TABLE exam_items (
    item_id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id             uuid NOT NULL REFERENCES exam_sections (section_id) ON DELETE CASCADE,
    question_id            uuid NOT NULL REFERENCES questions (question_id),
    ordinal                integer NOT NULL CHECK (ordinal >= 0),
    display_number_override text,
    score_override         double precision CHECK (score_override >= 0),
    UNIQUE (section_id, ordinal)
);

CREATE INDEX exam_items_question_idx ON exam_items (question_id);

-- A job has at most one exam; the reverse link is filled in the persistence
-- transaction. There is no cascade in either direction.
ALTER TABLE ingest_jobs
    ADD CONSTRAINT ingest_jobs_exam_fk FOREIGN KEY (exam_id) REFERENCES exams (exam_id);
