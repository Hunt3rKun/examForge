-- Global image assets. asset_id is the IR-compatible ID (ast_...); the row is
-- metadata only. Object bytes live in S3-compatible storage under storage_key.
-- All statements are transactional in PostgreSQL.

CREATE TABLE assets (
    asset_id           text PRIMARY KEY
        CHECK (asset_id ~ '^ast_[A-Za-z0-9_-]+$' AND char_length(asset_id) <= 128),
    media_type         text NOT NULL
        CHECK (char_length(media_type) BETWEEN 1 AND 128),
    storage_key        text NOT NULL UNIQUE,
    raw_storage_key    text,
    status             text NOT NULL
        CHECK (status IN ('raw_cropped', 'cleaned', 'vectorized')),
    aspect_ratio       double precision CHECK (aspect_ratio > 0),
    suggested_width_mm double precision CHECK (suggested_width_mm > 0),
    caption            text,
    alt                text,
    source_bbox        jsonb CHECK (source_bbox IS NULL OR jsonb_typeof(source_bbox) = 'object'),
    sha256             char(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Non-unique: equal content never implies one asset identity.
CREATE INDEX assets_sha256_idx ON assets (sha256);
