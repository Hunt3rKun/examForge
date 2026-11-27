-- Slice 4A: explicit asset variants. Before this migration the active object
-- was implied by whichever key happened to be in storage_key. Now each variant
-- has its own key and digest, and active_variant names the served one.
--
-- Backfill follows the slice-3 storage rules; no key or digest is fabricated:
--   raw_cropped          main = raw; no clean variant; active = raw
--   cleaned              main = clean; raw companion = raw; active = clean
--   legacy cleaned row   raw key kept with its NULL digest, so raw stays
--                        unverified and unavailable; active = clean
-- storage_key/sha256 are retained for compatibility and always mirror the
-- variant they described before this migration.

ALTER TABLE assets
    ADD COLUMN clean_storage_key text,
    ADD COLUMN clean_sha256 char(64)
        CHECK (clean_sha256 IS NULL OR clean_sha256 ~ '^[0-9a-f]{64}$'),
    ADD COLUMN active_variant text NOT NULL DEFAULT 'clean'
        CHECK (active_variant IN ('raw', 'clean')),
    ADD COLUMN revision bigint NOT NULL DEFAULT 1
        CHECK (revision > 0);

-- A clean digest without a clean key is meaningless; the converse is not
-- constrained because legacy rows may predate the digest contract.
ALTER TABLE assets
    ADD CONSTRAINT assets_clean_digest_needs_key
        CHECK (clean_sha256 IS NULL OR clean_storage_key IS NOT NULL);

-- raw_cropped: the single main object is the raw crop.
UPDATE assets
SET raw_storage_key = storage_key, raw_sha256 = sha256
WHERE status = 'raw_cropped' AND raw_storage_key IS NULL;

-- cleaned: the main object is the clean image; the raw companion keeps its own
-- key and digest (NULL digest on a legacy row stays NULL).
UPDATE assets
SET clean_storage_key = storage_key, clean_sha256 = sha256
WHERE status = 'cleaned';

UPDATE assets
SET active_variant = CASE WHEN status = 'raw_cropped' THEN 'raw' ELSE 'clean' END;
