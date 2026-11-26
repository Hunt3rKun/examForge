# Exam IR — current-state contract

## Implemented in Stage 0

- `schemas/exam-ir-v1.json`: exchange schema for exam metadata, globally unique assets, normalized bounding boxes, content blocks, recursive question items, and provenance.
- `schemas/layout-spec-v1.json`: independent renderer layout options.
- `internal/ir/`: Go DTOs and schema validation. See source for the exact supported surface.
- `testdata/golden/ir/sample.json`: golden exchange fixture.

`ExamDocumentIR` is an exchange/render-view DTO, not a database model. Future persistence must split global `assets` (unique `asset_id`), `questions`, and `exams` to support cross-exam deduplication and free-form composition. Do not make the DTO mirror those tables.

## Change rule

Schemas are authoritative and must be updated first when changing data shape. Keep this contract synchronized with actual schema, Go, and fixture paths. Inline parsing is planned for MVP slice 1 in Go `internal/ir/inline`; it is not implemented in Stage 0.
