# Pipeline and queue — current-state contract

## Implemented (MVP slice 2)

The ingest pipeline is PostgreSQL + River + S3-protocol object storage, fully
offline-testable through in-memory fakes. There is no HTTP API, frontend,
real VLM/OCR client, or DOCX output in this slice.

### Layout

- `migrations/0001_assets.sql` … `0004_exams.sql`: application schema. River
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
  `QuestionFingerprint` (`FingerprintVersion = 1`), `SourceRecord`,
  `VerifiedAsset`, `ExamRecord`, `AssetURIScheme = "examforge-asset://"`.
- `internal/store/storetest/`: `MemDB`, an in-memory `Workspace`/`IngestJobs`
  with snapshot rollback and fault injection, plus an in-memory `Persister`.
  It mirrors schema keys; real SQL is exercised only by integration tests.
- `internal/parser/`: `ParserAdapter` boundary (`Source`, `AssetBlob`,
  `ParseResult`, `Error` with permanent/transient classes), `ReadSource`, and
  `MockParserAdapter` with fault hooks (`FromGolden`, `InvalidTimes`,
  `FailTimes`, `DuplicateQuestions`, `Delay`, `Mutate`).
- `internal/pipeline/`: `Submitter` (bounded source read, `%PDF-` check,
  deterministic upload key, one transaction for the `ingest_jobs` row + River
  `InsertTx` + `river_job_id`), `IngestWorker` (River `WorkerDefaults`),
  `PGXRunner`/`RiverClient` adapters, `RiverMigrate`/`RiverVersion`,
  state machine, and permanent/transient classification.
- `cmd/exampipe/main.go`: operator CLI (environment-only configuration) that
  migrates, submits, works one job, assembles the stored exam and renders a
  PDF through `S3AssetResolver`. Never part of the offline gate.

### Schema

`assets` (`asset_id` PK, `storage_key` unique, `sha256`, status/geometry
checks), `questions` (`question_id` UUID PK, `content_json` complete
`QuestionItem`, `content_fingerprint` + `fingerprint_version`, non-unique
fingerprint index), `ingest_jobs` (`state` CHECK, `river_job_id` unique,
`source_file_sha256` non-unique), `exams` (one per `ingest_job_id`, unique),
`exam_sections` (unique `(exam_id, ordinal)` and `(exam_id, ir_section_id)`),
`exam_items` (unique `(section_id, ordinal)`, FK to `questions`), with
`ingest_jobs.exam_id` FK added in `0004`.

Asset rows are written with `INSERT ... ON CONFLICT (asset_id) DO NOTHING
RETURNING *` (`internal/store/queries/assets.sql`). Two concurrent ingests of
equal content both read no row and both insert; the loser gets zero rows back,
re-reads the winner's row, and applies the same SHA-256 and identity rules
(`sameAssetIdentity`). Same bytes and identity is idempotent; a different
SHA-256 or identity metadata is `ErrConflict` and never overwrites the
registered row. This keeps content-addressed asset registration idempotent
without a transaction-level lock.

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

## Boundaries

The `Submitter`, worker, parser, and object store sit behind interfaces so the
offline gate never needs PostgreSQL, S3, or network access. Future HTTP
handlers and a real parser replace the seams, not the state machine. Queue
families beyond ingestion (asset processing, rendering) are not implemented.
