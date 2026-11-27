# Pipeline and queue — current-state contract

## Implemented (MVP slices 2 + 3; slice 4A API/server)

The ingest pipeline is PostgreSQL + River + S3-protocol object storage, fully
offline-testable through in-memory fakes. Slice 3 adds the cloud vision parser
behind the same `ParserAdapter` seam and dual-object (raw + clean) asset
persistence. Slice 4A adds the store occurrence/variant primitives, a bounded
`net/http` API, OpenAPI contract, and resident `cmd/examserver` (details below).
The API remains an internal trusted-network service; it is not intended for
public exposure. No frontend or DOCX output is implemented.

### Layout

- `migrations/0001_assets.sql` … `0007_asset_variants.sql`: application
  schema. `0005` adds nullable `assets.raw_sha256` and the
  `assets_raw_digest_needs_key` CHECK. `0006_exam_item_occurrence.sql` adds
  nullable `exam_items.answer_space_mm_override` and positive `revision` (default
  1), backfilling positive legacy answer-space values from referenced question
  JSON; absent/zero values remain NULL and assemble as zero. `0007_asset_variants.sql`
  adds `clean_storage_key`, `clean_sha256`, `active_variant` (defaulted by
  status: `raw_cropped` → raw, otherwise clean), and positive asset `revision`.
  It backfills clean columns from the previous main object for cleaned assets and
  raw columns from the main object for raw-cropped assets; legacy raw keys without
  digest remain unverified. Existing `storage_key`/`sha256` are retained. River
  tables are never created here; `rivermigrate` owns them. `migrations.go`
  embeds `migrations/*.sql` as `examforge.Migrations` for the runner and the
  CLI.
- `internal/store/migrate/`: pure migration runner (embedded `fs.FS`, advisory
  lock, `examforge_schema_migrations` ledger, per-migration savepoints,
  SHA-256 checksums, post-apply equality check) plus `PgxDB` and `Up`/`Current`.
- `sqlc.yaml` + `internal/store/queries/*.sql` + generated
  `internal/store/sqlcgen/`: pinned sqlc v1.31.1, pgx/v5, `emit_interface`.
  Generated code is committed and `sqlc diff` is a gate.
- `internal/storage/`: `ObjectStore` boundary, canonical key rules
  (`ValidateKey`, `SourceKey`, `RawAssetKey`, `CleanAssetKey`), immutable puts
  (same key + same SHA idempotent, different bytes `ErrConflict`, never
  overwritten), bounded `Get`, `S3Store` (path-style forced, explicit
  `aws.Config`, proxy-free HTTP client, no `LoadDefaultConfig`), `MemStore`
  fake, `DeletePrefix`, error classes (`ErrNotFound`, `ErrForbidden`,
  `ErrInvalidKey`, `ErrTooLarge`, `ErrConflict`, `ErrInvalid`,
  `ErrUnavailable`), `Classify`, and `Redact`.
- `internal/store/`: `Repository` (pool, `InTx`, read-only `Read`,
  `GetJob`/`Claim`/`Move`/`Fail`, `AssetRecord`), error classes
  (`ErrNotFound`/`ErrConflict`/`ErrInvalid`) and `Translate` (SQLSTATE map:
  `23505`/`23503` → `ErrConflict`, `23502`/`23514`/`22001`/`22003`/`22P02` →
  `ErrInvalid`, everything else left for transient classification),
  `Assembler` (`Persist`/`PersistTx`/`Assemble`), `Persister`, canonical
  `QuestionFingerprint` (`FingerprintVersion = 2`; `LegacyFingerprintVersion = 1`),
  `SourceRecord`, `VerifiedAsset`, `ExamRecord`,
  `AssetURIScheme = "examforge-asset://"`. Fingerprint v2 excludes the
  occurrence-owned `answer_space_mm`; v1 rows remain version-isolated and are
  not rewritten.
- `internal/store/storetest/`: `MemDB`, an in-memory `Workspace`/`IngestJobs`
  with snapshot rollback and fault injection, plus an in-memory `Persister`.
  It mirrors schema keys; real SQL is exercised only by integration tests.
- `internal/parser/`: `ParserAdapter` boundary (`Source`, dual-variant
  `AssetBlob{RawData, CleanData, RawSHA256, SHA256}`, `ParseResult`, `Error`
  with permanent/transient classes), `ReadSource`, and `MockParserAdapter`
  with fault hooks (`FromGolden`, `InvalidTimes`, `FailTimes`,
  `DuplicateQuestions`, `Delay`, `Mutate`).
- `internal/parser/cloud/` + `internal/vision/raster/` + `internal/vision/clean/`:
  the slice-3 vision parser (see `docs/subsystems/vision-parser.md`). The
  worker consumes its output through the identical seam.
- `internal/pipeline/`: `Submitter` (bounded source read, `%PDF-` check,
  deterministic upload key, one transaction for the `ingest_jobs` row + River
  `InsertTx` + `river_job_id`), `IngestWorker` (River `WorkerDefaults` plus a
  `Timeout` override from `WorkerConfig.JobTimeout`), `PGXRunner`/`RiverClient`
  adapters, `RiverMigrate`/`RiverVersion`, state machine, and
  permanent/transient classification. Asset uploads are status-driven:
  `raw_cropped` writes one object (main key = raw key); `cleaned` writes two
  different objects with independently recomputed digests
  (`VerifiedAsset.SHA256` for clean, `RawSHA256` for raw); identical raw/clean
  bytes, missing variants, declared-digest mismatches and media-type
  mismatches are permanent `asset_invalid` failures.
- `cmd/exampipe/main.go`: operator CLI (environment-only configuration) that
  migrates, submits, works one job, assembles the stored exam and renders a
  PDF through `S3AssetResolver`. `-parser mock|cloud` selects the adapter
  explicitly; `cloud` requires the complete `EXAMFORGE_VLM_*` environment
  (checked before any service connection), takes `-parse-budget` and
  `-job-timeout` (the cloud config enforces budget + reserve < job timeout),
  and prints bounded parse diagnostics (pages, stage times, usage, repair
  rounds, warnings). The API key is never a flag and never printed. Never
  part of the offline gate.

### Slice 4A API and server

- `api/openapi.yaml` is the OpenAPI 3.1 transport contract; `cmd/openapicheck`
  checks its structure and local references offline. This is a structural
  reference checker, not a full OpenAPI validator or client generator.
- `internal/api/` implements the method/path routes for health/readiness, PDF
  ingest, paginated job/exam catalogs and detail, source-PDF proxy, occurrence
  item PATCH, asset variant proxy/selector PATCH, and immediate PDF rendering.
  DTOs keep item revisions and asset selector state outside Exam IR v1. The
  package owns request IDs, generic error envelopes, CORS allowlist, body caps,
  route deadlines/semaphores, bounded warnings, strong ETags, and access logs
  that use route templates rather than resource IDs.
- `internal/api/domain.go` adapts `Repository`/`Assembler` operations to the
  `Domain` seam. `internal/store/catalog.go` supplies catalog projections and
  consistent exam snapshots; `internal/store/edit.go` supplies occurrence
  edits with copy-on-write, revision checks, and active-variant updates.
- `cmd/examserver/` validates environment-only configuration, uses the
  explicit S3 client and pgx pool, refuses pending application/River migrations,
  configures producer always and worker optionally, composes parser/renderer/API,
  and performs readiness-first graceful shutdown with a shared deadline.
  It does not auto-migrate or source `.env` files.
- `migrations/0006_exam_item_occurrence.sql` and
  `migrations/0007_asset_variants.sql` establish the occurrence and asset
  revision/variant data model. `internal/storage/resolver.go` selects active,
  raw, or clean key+digest, rejects unavailable/unverified variants, verifies
  bytes, and creates an isolated resolver snapshot/cache for each render.

### Schema

`assets` (`asset_id` PK, `storage_key` unique, `sha256`, nullable
`raw_sha256` with the `assets_raw_digest_needs_key` CHECK, clean key/digest,
`active_variant`, positive `revision`, status/geometry checks), `questions`
(`question_id` UUID PK, `content_json` complete entity `QuestionItem`,
`content_fingerprint` + `fingerprint_version`, non-unique fingerprint index),
`ingest_jobs` (`state` CHECK, `river_job_id` unique, `source_file_sha256`
non-unique), `exams` (one per `ingest_job_id`, unique), `exam_sections` (unique
`(exam_id, ordinal)` and `(exam_id, ir_section_id)`), `exam_items` (unique
`(section_id, ordinal)`, FK to `questions`, occurrence `answer_space_mm_override`
and positive `revision`), with `ingest_jobs.exam_id` FK added in `0004`.

Asset rows are written with `INSERT ... ON CONFLICT (asset_id) DO NOTHING
RETURNING *` (`internal/store/queries/assets.sql`). Two concurrent ingests of
equal content both read no row and both insert; the loser gets zero rows back,
re-reads the winner's row, and applies the same SHA-256 and identity rules
(`sameAssetIdentity`). Same bytes and identity is idempotent; a different
SHA-256 or identity metadata is `ErrConflict` and never overwrites the
registered row. This keeps content-addressed asset registration idempotent
without a transaction-level lock.

Dual-digest rules (slice 3): a new `cleaned` record must carry both a raw
companion key and a valid raw digest; a `raw_cropped` record must carry
neither. When an existing row and the incoming record both declare a raw
digest, a difference is `ErrConflict`. A legacy row (`raw_sha256 IS NULL`)
keeps its stored shape — no digest is fabricated — and its main object remains
authoritative for the clean variant when applicable; `S3AssetResolver` refuses
a raw companion without its own registered digest and never verifies raw bytes
against the clean digest. Since migration `0007`, selection and availability are
explicit in the variant columns; the legacy main key/digest are retained for
compatibility.

### Ingest state machine

`queued → parsing → validating → persisted`, and any non-terminal state may
move to `failed`; `persisted` and `failed` are terminal. The worker claims from
`queued|parsing|validating` so a re-delivered attempt re-enters `parsing`.
Every later transition is a compare-and-set on the current state; a no-match
is `ErrNotFound`. The exam and the `persisted` job commit in one transaction
(`CompleteIngestJob` requires `validating`). Replaying a persisted job is a
no-op and never adds an exam.

### Failure classification

River owns the retry schedule; `ingest_jobs.state` records the domain outcome
and never duplicates the queue. Permanent errors (invalid IR, bad asset bytes,
source defects, store conflict/invalid, missing object, key rejection,
permanent parser errors) are recorded on the job as `failed` with a stable code
and the River job is cancelled (`river.JobCancel`), so it is never retried.
Transient errors are returned unchanged and River retries with backoff; the
active domain state is left intact. Stored messages are `storage.Redact`-ed
(whole URLs, AWS-key shapes, bearer/secret assignments, configured secrets) and
bounded at 2000 runes.

### Configuration and credentials

Database URL, S3 endpoint/bucket/region/access key/secret and
`EXAMFORGE_S3_FORCE_PATH_STYLE` come only from the environment. Path-style
addressing is mandatory; `false` or an unparseable value is rejected. Secrets
never appear in Go constants, command-line flags, fixtures, logs, or stored
error messages.

### Verification

- `scripts/verify.sh` (offline, default): pinned tools including `sqlc
  v1.31.1`, `gofmt`, `go vet`, `staticcheck`, `go test -race -count=1 ./...`,
  schema checks, `sqlc diff` drift gate, render smoke and network-cut renders.
- `scripts/verify-integration.sh` (explicit, never in the default gate):
  `go vet -tags=integration ./...` then `go test -tags=integration -race
  -count=1 ./...` with the `EXAMFORGE_TEST_DATABASE_URL`,
  `EXAMFORGE_TEST_S3_ENDPOINT`, `EXAMFORGE_TEST_S3_BUCKET`,
  `EXAMFORGE_TEST_S3_REGION`, `EXAMFORGE_TEST_S3_ACCESS_KEY_ID`,
  `EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY`,
  `EXAMFORGE_TEST_S3_FORCE_PATH_STYLE=true` contract. Missing or malformed
  values fail before any test runs; there is no fallback endpoint. The test
  binary enforces the same contract when run directly.
  `--env-file FILE` sources a local file first and refuses one readable by
  group/other; `--check-only` runs only `TestIntegrationPreflight` (connect,
  create/drop schema, apply both migration sets, round-trip one S3 object) and
  needs no C compiler because it skips `-race`.
- `scripts/setup-integration-env.sh`: interactive writer for that env file.
  It reads the PostgreSQL and S3 values (secrets never echoed), percent-encodes
  the database URL, writes mode-0600 `.env.integration` (git-ignored), prints
  only a masked summary plus a TCP reachability probe, and prints the
  provisioning reminders (role `CREATE` privilege, bucket creation).
- `internal/pipeline/integration_test.go` (`//go:build integration`) runs each
  test in its own PostgreSQL schema: full pipeline through a real River worker
  and Typst render, transactional rollback, duplicate-source jobs, shared
  question entities, transient retry, permanent cancel, and asset
  immutability. Upload objects are removed by a key snapshot diff in
  `t.Cleanup` on every path; content-addressed asset objects are immutable and
  shared, so they are left in place.
- `internal/pipeline/e2e_test.go` is the offline end-to-end proof: in-memory
  store and DB, mock parser, real Typst render through the resolver.
- `internal/pipeline/cloudE2E_test.go` extends that proof to slice 3: real
  mutool rasterization (or a synthetic dirty page), a localhost httptest VLM,
  the cloud parser, dual-object worker uploads, assembly and a real Typst
  render — for both the `raw_cropped` fallback and the `cleaned` path.
- `internal/pipeline/dualasset_test.go` and `internal/store/dualasset_test.go`
  cover the dual-payload rejection matrix, independent digest storage, raw
  digest conflicts and legacy-row behavior offline;
  `internal/storage/rawresolver_test.go` covers `ResolveRaw`.
- `internal/pipeline/dualasset_integration_test.go`
  (`//go:build integration`) proves against real PostgreSQL + S3: cleaned
  dual digests verify their own objects, the migration keeps legacy rows
  NULL with the main object readable and raw reads refused, and the
  `assets_raw_digest_needs_key` CHECK rejects a digest without a key.

## Boundaries

The `Submitter`, worker, parser, object store, API domain, and renderer sit
behind interfaces so the default offline gate never needs PostgreSQL, S3, or
network access. The cloud parser is one such seam implementation. Queue families
beyond ingestion (asset processing, rendering) are not implemented. River
retries re-run the whole parse: there is no cross-attempt page-result cache, so
a retried job may bill provider tokens again. The slice-4A contract and known
limitations remain documented in `docs/specs/slice-4a-http-api.md` and must be
kept aligned with `api/openapi.yaml` and the source routes.
