# Exam IR — current-state contract

## Implemented

- `schemas/exam-ir-v1.json`: exchange schema for exam metadata, globally unique assets, normalized bounding boxes, content blocks, recursive question items, and provenance. It bounds strings (`maxLength`), arrays (`maxItems`), assets/tables/question fan-out, table spans, and option labels (`minLength: 1`). `metadata.title` and `metadata.subtitle` are tokenized through the same inline parser as every other inline text field.
- `schemas/layout-spec-v1.json`: independent renderer layout options.
- `internal/ir/`: typed DTOs plus `Validator`, which compiles both schemas once, validates and decodes the same bytes (`DecodeExamIR`/`DecodeLayoutSpec`), and enforces semantic limits JSON Schema cannot express: 8 MiB Exam IR, question depth 4, 8192 questions, table columns 64 / rows 512 / cell slots 8192, `assets` key equals `asset_id`, option labels are non-empty plain text, and each question's `{{blank:N}}` numbering is unique and contiguous from 1. `Table.Headers` is typed `TableHeaders` and normalizes flat or nested exchange shapes at decode time. `ValidateExamSemantics` is also called by `Precompile`, so library callers cannot bypass bounds.
- `internal/ir/inline/`: deterministic Go inline tokenizer (MVP slice 1). `inline.Parse(source)` returns `Result{Tokens, Diagnostics}` for the supported inline subset: plain text, `$LaTeX$` (`math_inline`, raw LaTeX payload), `**bold**`, `*italic*`, `__underline__`, and `{{blank:N}}` (1-based index). Escapes (`\\`, `\$`, `\*`, `\_`) are honored; malformed markup degrades to literal text with a `Diagnostic` (code + byte range), never a panic or silent drop. Unsupported constructs stay literal. Style segments split by inline math never emit empty tokens. Table-driven tests: `internal/ir/inline/inline_test.go`.
- `cmd/ircheck/`: schema-validation CLI used by the verify gate.
- `testdata/golden/ir/sample.json`, `testdata/golden/layout/student.json`, `testdata/golden/layout/teacher.json`: golden exchange fixtures.
- `testdata/render/ir/` (`options.json`, `pages.json`) and `testdata/render/layouts/*.json`: render fixtures exercising option-grid measurement, paper sizes, binding line, header/footer, floats, tables, blanks, and answer-space reserves.

`ExamDocumentIR` is an exchange/render-view DTO, not a database model. Future persistence must split global `assets` (unique `asset_id`), `questions`, and `exams` to support cross-exam deduplication and free-form composition. Do not make the DTO mirror those tables.

## Change rule

Schemas are authoritative and must be updated first when changing data shape. Keep this contract synchronized with actual schema, Go, and fixture paths. Inline parsing lives only in Go `internal/ir/inline`; Typst templates consume precomputed tokens and must never re-parse inline markup. Rendering consumes the IR through `internal/render` — see `docs/subsystems/render-engine.md`.