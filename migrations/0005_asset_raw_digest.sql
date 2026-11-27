-- Slice 3: dual-object assets. A cleaned asset keeps two different objects
-- (clean main + raw companion); each needs its own digest so the resolver can
-- verify either independently. NULL is allowed so rows written before this
-- migration keep working: their raw companion, if any, predates the digest
-- contract and is not readable as verified raw content. No digest is
-- fabricated for historical rows.

ALTER TABLE assets
    ADD COLUMN raw_sha256 char(64)
        CHECK (raw_sha256 IS NULL OR raw_sha256 ~ '^[0-9a-f]{64}$');

-- A digest without a companion key is meaningless. The converse is NOT
-- constrained: a pre-migration cleaned row may hold a raw key with a NULL
-- digest, and such a row keeps its legacy behavior (the raw companion is not
-- readable as verified content). Requiring both for NEW cleaned rows is the
-- assembler's job, where the distinction between new and historical exists.
ALTER TABLE assets
    ADD CONSTRAINT assets_raw_digest_needs_key
        CHECK (raw_sha256 IS NULL OR raw_storage_key IS NOT NULL);