-- Reusable question entities. content_json is a complete, self-contained
-- QuestionItem; exam occurrence data lives in exam_items.

CREATE TABLE questions (
    question_id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject             text NOT NULL
        CHECK (char_length(subject) BETWEEN 1 AND 65536),
    grade_level         text,
    kind                text NOT NULL CHECK (kind IN ('single', 'composite')),
    question_type       text,
    knowledge_points    text[] NOT NULL DEFAULT '{}',
    difficulty          double precision CHECK (difficulty >= 0 AND difficulty <= 1),
    content_fingerprint char(64) NOT NULL
        CHECK (content_fingerprint ~ '^[0-9a-f]{64}$'),
    fingerprint_version smallint NOT NULL CHECK (fingerprint_version > 0),
    content_json        jsonb NOT NULL CHECK (jsonb_typeof(content_json) = 'object'),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Non-unique: equal fingerprints may belong to several entities.
CREATE INDEX questions_fingerprint_idx ON questions (content_fingerprint, fingerprint_version);
CREATE INDEX questions_knowledge_points_idx ON questions USING gin (knowledge_points);
CREATE INDEX questions_subject_grade_type_idx ON questions (subject, grade_level, question_type);
