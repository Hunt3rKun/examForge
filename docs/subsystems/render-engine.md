# Render engine — current-state contract

## Implemented (MVP slice 1)

The renderer contract is `Render(ctx, ExamDocumentIR, LayoutSpec) -> *Artifact` (`internal/render/render.go`, `Renderer` interface). `Render` re-validates both DTOs through `ir.Validator` before preprocessing, so library callers cannot bypass schema or semantic limits. Layout policy comes only from the Layout Spec; command-line flags select inputs/outputs, never layout. `Artifact{Bytes, MIMEType, Warnings}` is self-contained; warnings carry an IR path (`RenderWarning{Code, Path, Message}`). Failures wrap sentinel classes (`ErrInvalidInput`, `ErrAsset`, `ErrAssetTooLarge`, `ErrPackage`, `ErrTemplate`, `ErrCompileFailed`, `ErrRenderTimeout`, `ErrQueueTimeout`) via `RenderError{Op, Err, Stderr}` for `errors.Is` classification.

### Pipeline (`internal/render/`)

1. **Precompile** (`precompile.go`, `Precompile`): pure, no I/O. Runs `ir.ValidateExamSemantics` and the 8 MiB serialized-size bound, then tokenizes every inline field through `internal/ir/inline` (including exam title/subtitle, section title/instruction, and header/footer); preserves ordered section boundaries and compiles each section's ordered root questions, assigning globally pre-order question indexes across sections and nested children. Computes each question's `display_label` in Go (ASCII number/letter gets `.`, existing Chinese/bracketed punctuation is preserved); resolves layout constants (`layout.go`); normalizes tables into rectangular grids with explicit spans (`table.go`, bounded at 64 columns / 512 rows / 8192 slots, overlap/impossible grids are errors); groups a stem `float_right` image with all adjacent text blocks into a local side-by-side `float_group` (fallback: right-aligned block image + `float_fallback` warning); computes the mode truth table (`student`/`teacher` × `show_answer`/`show_analysis`; `reserve_space_mm` only for student without shown answers); lists each referenced asset once, sorted by ID. Warnings use paths rooted at `sections[i].questions[j]`. Unknown blocks render a visible placeholder plus `unknown_block` warning — never silently dropped.
2. **Payload** (`payload.go`): versioned `RenderPayload` JSON (`payload.json`), the only thing the template reads. Passes: `measure` and `render`.
3. **Assets** (`assets.go`): `LocalAssetResolver` reads `manifest.json` (asset_id → relative path) under a root. IDs and manifest paths are validated (no traversal, absolute paths, backslashes, NUL, symlinks escaping the root); the IR `uri` is never opened as a path. Copies are capped per asset (32 MiB) and per render (64 MiB), decoded-header-validated before Typst: declared MIME must match PNG/JPEG/GIF content, raster images are capped at 40M decoded pixels, WebP magic is checked, and SVG must be smaller than 8 MiB with bounded XML depth/elements and no DOCTYPE. Bytes are hashed (SHA-256) and staged with bounded parallelism.
4. **Workspace + subprocess** (`workspace.go`, `process.go`, `typst.go`): `TypstRenderer` (`NewTypstRenderer(TypstConfig)`) runs each render in a fresh temp workspace (`--root .`), with the vendored packages staged read-only under `packages/` and an empty `package-path` shadowing any user package directory. The child environment is a whitelist (`PATH`, locale, Windows essentials) plus blackholed proxies; `HOME`, `TMPDIR` and `XDG_CACHE_HOME` point into the workspace. On Linux, Typst is launched through `prlimit --as=2GiB --` (configurable via `MemoryLimitBytes`) so a hostile image or SVG cannot exhaust host memory; the injected `CommandFactory` test path is not wrapped. Typst runs with vendored fonts only (`--font-path third_party/fonts --ignore-system-fonts`), `--creation-timestamp 0`, and `--jobs 1`, making repeated renders byte-identical. The constructor verifies the font SHA-256 and probes `typst --version` / `typst fonts` against the pinned 0.15.x/family. Concurrency is bounded by a semaphore (default `runtime.NumCPU()`); the renderer deadline (default 30s, caller deadlines obeyed) starts before the semaphore wait, so queueing is bounded and reports `ErrQueueTimeout`. Every invocation uses `context.WithTimeout` plus `WaitDelay`; stderr is bounded and workspace paths scrubbed; workspaces are removed on every path including cancellation. `CommandFactory` injects fake/slow commands for tests through the identical production path.
5. **Two-pass option grids**: the measure pass compiles once and publishes per-group option widths as Typst metadata (`typst eval --format json`); Go applies the uniform ladder `1x4 → 2x2 → 4x1` (`chooseOptionGrid`) from real measured widths; the render pass compiles with resolved `option_columns`. Explicit grids (`1x4`/`2x2`/`4x1` in `option_layout`) skip measurement.
6. **Vendored packages** (`vendored.go`): only `@preview/mitex:0.2.7` from `third_party/typst-packages/` is allowed (import allowlist for template files); every vendored file is checked against the `mitexFiles` SHA-256 manifest before staging; missing/tampered/extra files fail with `ErrPackage`. See `third_party/README.md` for provenance.

### Template (`templates/exam.typ`)

Purely declarative: consumes `payload.json` only. No Markdown/LaTeX/blank parsing, no regex logic, no asset URI resolution, no raw IR/Layout JSON. Renders ordered section headings and optional instructions before each section's questions; section and exam text arrive as inline tokens. Also renders inline tokens (MiTeX `mi`/`mitex` for math), option grids, blank underlines with queryable metadata, tables with `rowspan`/`colspan`, local side-by-side float groups, centered display math, binding line (page background, left margin reserve), header/footer running lines, paper sizes a4/a3/jis-b4, 1/2 columns, and mode-driven answer/analysis parts. Token text is read defensively (`at("text", default: "")`); question labels come precomputed as `display_label`; option-label punctuation is the only remaining display-only template logic. Widths that become numbers stay single-unit absolute lengths (`number-gap = 5pt`).

### CLI and gate

- `cmd/examrender/main.go`: `examrender -ir <exam.json> -layout <layout.json> -assets <dir> -out <file.pdf>` with optional `-schemas`, `-templates`, `-packages`, `-typst`, `-timeout`, `-check` (validate + precompile only). Schemas are compiled once; inputs are validated and decoded from the same bytes; the output may not overwrite any input or symlink target; output is written atomically with mode 0644, fsync and directory fsync; warnings go to stderr.
- `scripts/verify.sh`: offline gate — pinned Typst 0.15.x, vendored font, and sqlc v1.31.1 checks, `gofmt -l`, `go vet`, `staticcheck`, `go test -race -count=1 ./...`, `ircheck` on golden and render fixtures, `sqlc diff` drift gate, `examrender -check`, student/teacher renders with `pdfinfo` assertions, byte-identical repeated render, then a network-cut (`unshare -r -n`) offline re-render stage. `.github/workflows/verify.yml` runs the same gate with pinned tool versions.
- Physical tests: `internal/render/typst_real_test.go` asserts real PDF geometry via `pdftotext -bbox`/`pdfinfo` (grid ladder placement, paper sizes, binding position, per-page header/footer, float column narrowing, student/teacher reserve gap, offline render, workspace zero-residue). Fake-process tests: `internal/render/process_test.go` (semaphore bound, cancellation, timeout, stderr truncation, orphan cleanup).

### Asset resolver for persisted exams (slice 2)

`internal/storage/resolver.go` `S3AssetResolver` implements `render.AssetResolver`
over the object store and the asset registry: it reads only the asset ID and
declared media type from the IR (`uri`/`raw_uri` are never opened), resolves the
registered active variant's key and digest, streams the object through a
digest-verifying, size-bounded reader, and maps store failures to
`render.ErrAsset`/`render.ErrAssetTooLarge` while keeping transient
`storage.ErrUnavailable` recognizable. `ForRender()` returns a render-scoped
resolver that downloads each asset at most once (singleflight, bounded cache),
so repeated references do not re-fetch. `TypstRenderer.Render` creates a fresh
scope per call when the configured resolver implements `RenderScopedResolver`;
`cmd/exampipe`, the resident API renderer, and `internal/pipeline/e2e_test.go`
render persisted exams through this boundary.

Slice 3: `AssetRecord` additionally carries `RawStorageKey`/`RawSHA256`, and
`ResolveRaw` opens the raw companion object verified against its own
registered digest. A companion without a digest (a pre-migration row) is
refused — the clean digest never verifies raw bytes. Migration
`migrations/0007_asset_variants.sql` adds explicit clean key/digest, active
selector, and revision while retaining the legacy main columns. The resolver
now supports `ResolveVariant` for `active`, `raw`, and `clean`; unavailable or
unverified variants are rejected. `Resolve` follows the registry's active
selector. Its render-scoped resolver snapshots each asset record once per
render and keys cached bytes by asset ID, variant and registered object identity,
so one render does not mix selector states. Slice 4A also exposes the active/raw/clean asset proxy and
revision-guarded active-variant endpoint through `internal/api/routes.go`; the
API-facing contracts and lifecycle are recorded in
`docs/subsystems/pipeline-queue.md`.

### Browser source and render preview (slice 4B)

- `web/src/features/workbench/PdfPane.tsx` and `web/src/pdf/` load complete PDF
  bytes from the source proxy, draw a bounded canvas for the current page,
  and project the selected question's provenance boxes into the viewport.
  The viewer exposes page navigation, zoom, rotation, and a page-only fallback
  when precise boxes are unavailable. It cancels obsolete work and releases
  the canvas and PDF.js document on teardown.
- `web/src/render/layout-presets.ts` provides student A4, teacher A4, and
  two-column A3 LayoutSpecs. `web/src/features/workbench/RenderPreview.tsx`
  submits the selected LayoutSpec to the HTTP render endpoint for the saved
  exam; unsaved edits block preview and download. The preview shows bounded
  warning summaries and uses the same PDF.js viewer. `web/src/render/render-client.ts`
  manages request cancellation and generated PDF Blob URL cleanup. Typst
  still determines final layout; the browser displays its PDF bytes.
- `web/src/main.tsx` bundles KaTeX CSS/fonts and the pinned PDF.js worker.
  `web/src/pdf/document-loader.ts` loads packed CMaps and standard fonts from
  same-origin `pdfjs/` paths; `web/vite.config.ts` serves them in development
  and emits them into `web/dist` in production. `scripts/verify-web.sh`
  checks the built assets and rejects external resource URLs.
- **Not tried for Web 4B:** real-browser offline asset loading, visual
  inspection of a Chinese/math PDF and high-DPR bounding-box alignment, or
  real-service preview and download.

### Fixtures

- `testdata/golden/assets/` (`manifest.json`, `sample_fig.png`): golden assets.
- `testdata/render/assets/` (`manifest.json`, `fig.png`): render-fixture assets.
- `testdata/render/ir/`, `testdata/render/layouts/`: option-grid, paper/binding/header-footer, float, table, blank, and answer-space fixtures (see `docs/subsystems/exam-ir.md`).

## Boundaries

The store layer assembles occurrence answer-space into Exam IR; see
`docs/subsystems/exam-ir.md` and `docs/subsystems/pipeline-queue.md`. Slice 4A
uses the existing renderer and asset resolver through the HTTP service; its
route and lifecycle contract is documented in the pipeline subsystem page.
Inline parsing belongs to Go `internal/ir/inline`; templates own layout only.
Future expansion is through the `Renderer` interface (e.g. `DocxRenderer`,
`LaTeXRenderer`) and the `AssetResolver` interface. Implemented resolvers are
`LocalAssetResolver` (manifest under a directory) and `S3AssetResolver`
(registry + object store); both are selectable by the caller without changing
the renderer. No CGO; document tools are invoked through `os/exec` wrappers.
Any external process must use `context.WithTimeout` and clean temporary files.
