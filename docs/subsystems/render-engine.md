# Render engine — current-state contract

## Implemented (MVP slice 1)

The renderer contract is `Render(ctx, ExamDocumentIR, LayoutSpec) -> *Artifact` (`internal/render/render.go`, `Renderer` interface). Layout policy comes only from the Layout Spec; command-line flags select inputs/outputs, never layout. `Artifact{Bytes, MIMEType, Warnings}` is self-contained; warnings carry an IR path (`RenderWarning{Code, Path, Message}`). Failures wrap sentinel classes (`ErrInvalidInput`, `ErrAsset`, `ErrAssetTooLarge`, `ErrPackage`, `ErrTemplate`, `ErrCompileFailed`, `ErrRenderTimeout`) via `RenderError{Op, Err, Stderr}` for `errors.Is` classification.

### Pipeline (`internal/render/`)

1. **Precompile** (`precompile.go`, `Precompile`): pure, no I/O. Tokenizes every inline field through `internal/ir/inline`; resolves layout constants (`layout.go`); normalizes tables into rectangular grids with explicit spans (`table.go`, overlap/impossible grids are errors); groups a stem `float_right` image with all adjacent text blocks into a local side-by-side `float_group` (fallback: right-aligned block image + `float_fallback` warning); computes the mode truth table (`student`/`teacher` × `show_answer`/`show_analysis`; `reserve_space_mm` only for student without shown answers); lists each referenced asset once, sorted by ID. Unknown blocks render a visible placeholder plus `unknown_block` warning — never silently dropped.
2. **Payload** (`payload.go`): versioned `RenderPayload` JSON (`payload.json`), the only thing the template reads. Passes: `measure` and `render`.
3. **Assets** (`assets.go`): `LocalAssetResolver` reads `manifest.json` (asset_id → relative path) under a root. IDs and manifest paths are validated (no traversal, absolute paths, backslashes, NUL, symlinks escaping the root); the IR `uri` is never opened as a path. Copies are size-capped, hashed (SHA-256), and staged into the workspace `assets/` directory with bounded parallelism.
4. **Workspace + subprocess** (`workspace.go`, `process.go`, `typst.go`): `TypstRenderer` (`NewTypstRenderer(TypstConfig)`) runs each render in a fresh temp workspace (`--root .`), with the vendored packages staged read-only under `packages/` and an empty `package-path` shadowing any user package directory; child env strips `TYPST_*` and blackholes proxies. Concurrency is bounded by a semaphore (default `runtime.NumCPU()`); every invocation uses `context.WithTimeout` (default 30s, caller deadlines always obeyed) plus `WaitDelay`; stderr is bounded and workspace paths scrubbed; workspaces are removed on every path including cancellation. `CommandFactory` injects fake/slow commands for tests through the identical production path.
5. **Two-pass option grids**: the measure pass compiles once and publishes per-group option widths as Typst metadata (`typst eval --format json`); Go applies the uniform ladder `1x4 → 2x2 → 4x1` (`chooseOptionGrid`) from real measured widths; the render pass compiles with resolved `option_columns`. Explicit grids (`1x4`/`2x2`/`4x1` in `option_layout`) skip measurement.
6. **Vendored packages** (`vendored.go`): only `@preview/mitex:0.2.7` from `third_party/typst-packages/` is allowed (import allowlist for template files); every vendored file is checked against the `mitexFiles` SHA-256 manifest before staging; missing/tampered/extra files fail with `ErrPackage`. See `third_party/README.md` for provenance.

### Template (`templates/exam.typ`)

Purely declarative: consumes `payload.json` only. No Markdown/LaTeX/blank parsing, no regex logic, no asset URI resolution, no raw IR/Layout JSON. Renders inline tokens (MiTeX `mi`/`mitex` for math), option grids, blank underlines with queryable metadata, tables with `rowspan`/`colspan`, local side-by-side float groups, centered display math, binding line (page background, left margin reserve), header/footer running lines, paper sizes a4/a3/jis-b4, 1/2 columns, and mode-driven answer/analysis parts. Widths that become numbers stay single-unit absolute lengths (`number-gap = 5pt`).

### CLI and gate

- `cmd/examrender/main.go`: `examrender -ir <exam.json> -layout <layout.json> -assets <dir> -out <file.pdf>` with optional `-schemas`, `-templates`, `-packages`, `-typst`, `-timeout`, `-check` (validate + precompile only). Inputs are schema-validated before render; the output may not overwrite any input; output is written atomically (temp file + rename); warnings go to stderr.
- `scripts/verify.sh`: offline gate — tool checks, `gofmt -l`, `go vet`, `go test -race -count=1 ./...`, `ircheck` on golden and render fixtures, `examrender -check`, student/teacher renders with `pdfinfo` assertions, then a network-cut (`unshare -r -n`) offline re-render stage.
- Physical tests: `internal/render/typst_real_test.go` asserts real PDF geometry via `pdftotext -bbox`/`pdfinfo` (grid ladder placement, paper sizes, binding position, per-page header/footer, float column narrowing, student/teacher reserve gap, offline render, workspace zero-residue). Fake-process tests: `internal/render/process_test.go` (semaphore bound, cancellation, timeout, stderr truncation, orphan cleanup).

### Fixtures

- `testdata/golden/assets/` (`manifest.json`, `sample_fig.png`): golden assets.
- `testdata/render/assets/` (`manifest.json`, `fig.png`): render-fixture assets.
- `testdata/render/ir/`, `testdata/render/layouts/`: option-grid, paper/binding/header-footer, float, table, blank, and answer-space fixtures (see `docs/subsystems/exam-ir.md`).

## Boundaries

Inline parsing belongs to Go `internal/ir/inline`; templates own layout only. Future expansion is through the `Renderer` interface (e.g. `DocxRenderer`, `LaTeXRenderer`) and the `AssetResolver` interface (e.g. network stores); this slice delivers only local assets and Typst PDF. No CGO; document tools are invoked through `os/exec` wrappers. Any external process must use `context.WithTimeout` and clean temporary files.