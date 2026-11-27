# Lessons

Dynamic engineering retrospective log. Record each lesson using this fixed format:

```markdown
### YYYY-MM-DD — <short lesson title>
- **Rule summary:** <actionable rule>
- **Pitfall and commit:** <why it failed; commit hash or `uncommitted`>
- **Reproduction and verification:** <steps/commands and expected result>
- **Seen N×:** <count> (dates: YYYY-MM-DD, ...)
```

When the same class of lesson appears on three different dates, propose promoting it into `docs/code-standards.md` and link the permanent rule here. Do not inflate counts for repeated observations on one date.

### 2026-11-26 — Injection seams must be honored by the constructor, not just declared
- **Rule summary:** When a config struct exposes an injection seam (e.g. `CommandFactory`), the constructor must actually use it; add a fake-injection test that fails if the seam is ignored, and prefer env-independent fakes (generated shell scripts) because the production path may overwrite `cmd.Env` after the factory returns.
- **Pitfall and commit:** `NewTypstRenderer` declared `cfg.CommandFactory` but always built its own `exec.CommandContext` wrapper, so process tests silently exercised real typst; a test-binary re-exec fake also failed because the renderer assigns `cmd.Env` after the factory returns. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/render/ -run 'TestRenderFakeSuccess|TestRenderSemaphoreBound'` — fake-script tests observe start/end activity files and pass only when the injected factory is used.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Typst content position prints arrays as code; visual inspection catches it
- **Rule summary:** In Typst templates, an array placed in content position renders as its code representation — always `.join()` mapped content. Assert geometry in tests AND rasterize/inspect at least one PDF per template change; text-only assertions missed both the array-as-code bug and justification stretching text past a narrowed float column.
- **Pitfall and commit:** `block.blocks.map(render-block)` in the float grid cell printed `par(body: [...])` literally; after `.join()`, `justify: true` stretched beside-image CJK text ~5pt past the narrowed column. Both were invisible to pdftotext-based tests until the PNG was inspected. `uncommitted`
- **Reproduction and verification:** Render `testdata/render/ir/pages.json` with `pages-nobinding.json` via `go run ./cmd/examrender`, `pdftoppm -png -r 80 out.pdf probe`, inspect: beside-image paragraphs stay inside the narrowed column and no code text appears; `TestRenderFloatSideBySide` asserts word x-bounds.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Geometry probes must measure across the feature, not beside it
- **Rule summary:** When asserting a layout feature (reserve space, indent, gutter) from PDF word coordinates, pick probe words that straddle the feature's actual render position; verify probe placement against a fresh render before trusting a failing/passing assertion. Stale PDF dumps from earlier test runs mislead diagnosis.
- **Pitfall and commit:** The student/teacher reserve test measured 填空→样例 inside one stem, but `reserve_space_mm` renders after the whole stem, so both modes gave identical 55.2pt gaps; a distance-to-page-bottom probe also failed because trailing whitespace is unobservable without following content. Fixed by adding a probe question pair (Q3 reserve 30mm, Q4 follower) and asserting the stem-to-stem gap difference. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/render/ -run TestRenderStudentTeacherDifference` passes; student gap − teacher gap ≥ 30mm − 2pt.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Build AWS SDK v2 clients from an explicit aws.Config, never from the ambient environment
- **Rule summary:** In a codebase whose default gate must never touch the network or ambient credentials, construct S3 clients with `aws.Config` (static credentials, explicit region, `HTTPClient` with `Proxy: nil`, `DefaultsModeStandard`) and `s3.NewFromConfig`; do not use `config.LoadDefaultConfig` (env/shared-config/IMDS) or `awshttp.NewBuildableClient` (honors `HTTP_PROXY`). `s3.New(s3.Options{...})` with a nil `HTTPClient` panics at call time, not construction.
- **Pitfall and commit:** The first S3 client used `s3.New` and panicked on the first request; the next attempt inherited a proxy from the environment, so offline tests tried to reach the network through a proxy. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/storage/` — the httptest stub records `Method host path`; a proxy-derived request fails, and the stub never sees a query string or a virtual-hosted bucket host.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — A drift gate is only real after you prove it fails
- **Rule summary:** When adding a generated-code gate (`sqlc diff`), mutate a generated file, confirm the gate exits non-zero, then restore and confirm it exits zero. Same for "fails when the tool is missing": exercise the empty-environment path of a new script rather than assuming the guard runs.
- **Pitfall and commit:** `sqlc diff` looks like a no-op (silent, exit 0) until proven otherwise; a gate wired without a mutation check can pass because it never actually compares. `uncommitted`
- **Reproduction and verification:** append a comment to `internal/store/sqlcgen/querier.go` → `sqlc diff` exits 1; restore → exits 0. `./scripts/verify-integration.sh` with no env prints `missing environment variables: ...` and exits 1; with `EXAMFORGE_TEST_S3_FORCE_PATH_STYLE=false` it fails before any test.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Test fakes that import the package under test force an external test package
- **Rule summary:** When a test helper package (e.g. `internal/store/storetest`) imports the package it fakes, in-package tests that import the helper create an import cycle. Move those tests to `package <pkg>_test` and qualify exported names; keep the fake out of the production import graph.
- **Pitfall and commit:** `internal/store/assembler_test.go` was `package store` and imported `storetest`, which imports `store` — the compiler rejects the cycle. Fixed by moving the file to `package store_test` with `store.` prefixes. `uncommitted`
- **Reproduction and verification:** `go build ./...` and `go test -race -count=1 ./internal/store/` pass only with the external test package; reintroducing the in-package import reproduces the cycle error.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-27 — An in-memory fake cannot stand in for driver encoding and SQL concurrency
- **Rule summary:** Treat the fake as a shape check only. Before claiming a persistence path works, run it against the real service and watch for (1) driver type encoding — pgx sends a nil `[]string` as SQL NULL, so `NOT NULL DEFAULT '{}'` does not apply to an explicit NULL; (2) SQLSTATE classification — an unmapped code (e.g. `23502`) makes a permanent data defect look transient and retries forever; (3) read-then-insert races — two transactions can both see no row, so insert must be `ON CONFLICT DO NOTHING` plus a re-read, not a bare `INSERT`. Mirror each defect in the fake (nil rejection, `RaceAsset` hook) and prove the offline test fails when the fix is reverted.
- **Pitfall and commit:** `MemDB` accepted a nil `KnowledgePoints` and a duplicate `InsertAsset` returned a conflict, so the offline gate passed while the real pipeline stalled in `validating` on `null value in column "knowledge_points"` and then failed concurrent duplicate submits with `unique constraint assets_pkey`. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/store/ -run 'TestConcurrentAssetInsertIsIdempotent|TestTranslateClassifiesPostgresErrors'` passes; deleting the lost-race re-read makes it fail with `insert asset ...: store: record not found`. Full path: `./scripts/verify-integration.sh --env-file .env.integration` — `TestIntegrationDuplicateSourceJobsBothPersist` and `TestIntegrationTransientRetry` pass only against real PostgreSQL.
- **Seen 1×:** (dates: 2026-11-27)
