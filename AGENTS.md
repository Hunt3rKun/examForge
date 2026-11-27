# ExamForge (铸卷)

Stage 0 repository for converting source exams into a reusable, renderer-neutral Exam IR and rendering it with explicit layout specifications. This index is intentionally lightweight; subsystem files are the current-state contracts, while `docs/specs/` contains proposals only. Never mix proposals into current-state documentation.

## Directory and documentation routes

| Before changing | Read current-state contract | Main paths |
|---|---|---|
| IR / schemas | `docs/subsystems/exam-ir.md` | `schemas/`, `internal/ir/`, `testdata/golden/ir/` |
| Rendering / templates | `docs/subsystems/render-engine.md` | `templates/`, renderer code, render fixtures |
| Queues / pipeline / persistence / storage | `docs/subsystems/pipeline-queue.md` | `internal/pipeline/`, `internal/store/`, `internal/storage/`, `migrations/`, `sqlc.yaml`, `cmd/exampipe/` |
| Vision / parsing | `docs/subsystems/vision-parser.md` | `internal/parser/` |
| Cross-cutting conventions | `docs/code-standards.md` | all code |

Update the relevant `docs/subsystems/*.md` current-state contract with every code change and verify its paths against source. Put proposed designs in `docs/specs/` only.

## Architecture invariants

- **No CGO**, including transitive dependencies. Invoke `mutool`/`pdftoppm`, `typst`, and `pandoc` through `os/exec` wrappers for document operations.
- Schema first: change `schemas/*.json` before changing corresponding data structures.
- Rendering has the dual-input contract `Render(ExamDocumentIR, LayoutSpec) -> Artifact`; never use scattered CLI flags as layout policy. Keep content independent of layout.
- Go owns deterministic inline Markdown / `$LaTeX$` / `{{blank:N}}` parsing (`internal/ir/inline` in MVP slice 1); never implement complex parsing regexes in `.typ` templates.
- Every subprocess uses `context.WithTimeout` and cleans temporary files. External AI, parsing, and rendering capabilities are behind interfaces with offline mocks.
- Exchange DTOs are separate from persistence: storage uses `assets` (globally unique `asset_id`), `questions`, `exams`/`exam_sections`/`exam_items`, and `ingest_jobs`; River owns its own tables via `rivermigrate`.
- The default gate (`./scripts/verify.sh`) is offline: no PostgreSQL, S3, or network. Real-service tests run only through `scripts/verify-integration.sh` with the `EXAMFORGE_TEST_*` environment contract and never fall back to a default endpoint.

## Verification and reporting

Before claiming any code change complete, actually run `./scripts/verify.sh`. Report the exact commands run and their actual results; explicitly list unverified items under **Not tried**. Never claim an unrun check passed.
