-- name: GetAsset :one
SELECT * FROM assets WHERE asset_id = $1;

-- Concurrent ingests of equal content may race here: both read no row, then
-- both insert. A DO NOTHING conflict returns zero rows, and the caller
-- re-reads and applies the same SHA-256 and metadata rules. That keeps "same
-- asset_id + same SHA is idempotent" true under concurrency without a
-- transaction-level lock.
-- name: InsertAsset :one
INSERT INTO assets (
    asset_id, media_type, storage_key, raw_storage_key, status,
    aspect_ratio, suggested_width_mm, caption, alt, source_bbox, sha256, raw_sha256
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (asset_id) DO NOTHING
RETURNING *;

-- name: ListAssets :many
SELECT * FROM assets WHERE asset_id = ANY(sqlc.arg(asset_ids)::text[]) ORDER BY asset_id;

-- name: UpdateAssetDisplay :execrows
UPDATE assets
SET caption = COALESCE(NULLIF(sqlc.arg(caption)::text, ''), caption),
    alt = COALESCE(NULLIF(sqlc.arg(alt)::text, ''), alt),
    updated_at = now()
WHERE asset_id = $1;

-- Clean up after an abandoned ingest job: only rows this job created.
-- name: DeleteAssets :execrows
DELETE FROM assets WHERE asset_id = ANY(sqlc.arg(asset_ids)::text[]);
