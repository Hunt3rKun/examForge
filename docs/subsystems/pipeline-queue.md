# Pipeline and queue — current-state contract

**Status:待实现。** No HTTP service, database migrations, or queue workers are implemented in Stage 0.

Planned boundaries only: ingest/source registration → parse/OCR → normalize Exam IR → review → render/export. Future queue jobs should have explicit states such as `queued`, `running`, `succeeded`, `failed`, and `cancelled`, with retry/idempotency boundaries defined before implementation. Preset queue families: ingestion, parsing/normalization, asset processing, and rendering. The selected future queue technology is River; persistence is PostgreSQL. Keep proposals and state-machine decisions in `docs/specs/` until implemented.
