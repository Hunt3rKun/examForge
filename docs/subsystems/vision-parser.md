# Vision and parser — current-state contract

## Implemented (MVP slices 2 + 3)

`internal/parser` is the boundary between a source PDF and a candidate Exam
IR. An adapter reads source bytes and returns the candidate document plus the
asset bytes it references; it never writes a database, uploads objects, or
enqueues jobs. The ingest worker validates the output and owns every side
effect.

### Contract

- `Source{JobID, StorageKey, SHA256, Open}`: `Open` returns a bounded,
  context-honoring stream. `ReadSource` reads and verifies it (size bound
  `storage.MaxSourceBytes`, digest match) before parsing.
- `AssetBlob{AssetID, MediaType, RawData, CleanData, RawSHA256, SHA256}`: one
  payload per IR asset. `RawData` is always required; `CleanData` is present
  only for `cleaned` assets and must differ from the raw bytes. Declared
  digests are claims: the worker recomputes both and rejects a mismatch as a
  permanent input failure.
- `ParseResult{Exam, Assets}`: the candidate IR and its asset bytes. The IR
  may carry bounded top-level `warnings` (exchange metadata, not persisted).
- `ParserAdapter.Parse(ctx, Source) (ParseResult, error)`.
- `parser.Error{Permanent, Code, Err}`: permanent marks input defects that must
  not be retried; anything else is transient. The pipeline maps permanent
  parser errors to the `parser_<code>` failure code.

### Adapters

`MockParserAdapter` (`internal/parser/mock.go`) replays a golden Exam IR and
its asset files for any valid PDF source, keeping the real contract (bounded
hashed read, candidate IR, raw payloads with declared digests). The asset
manifest, never the IR `uri`, decides which file an asset ID names. Fault
hooks for tests: `FromGolden`, `InvalidTimes`, `FailTimes`,
`DuplicateQuestions`, `Delay`, `Mutate`; `Calls`, `InjectInvalid`,
`InjectFailures` adjust state at runtime.

`CloudAPIParser` (`internal/parser/cloud/`) implements the same boundary
against an OpenAI-compatible multimodal Chat Completions endpoint. Stage
order is fixed and cannot be reordered:

1. `parser.ReadSource` (bounded, hashed, `%PDF-` checked).
2. Rasterize all pages locally at high DPI (`internal/vision/raster`).
3. Build one bounded JPEG transfer copy per page (proportional downscale,
   long edge ≤ 2048 px default, quality and byte ceiling configurable). The
   high-resolution originals never leave the process and are the only crop
   source.
4. Concurrent per-page extraction (`errgroup.SetLimit` + a shared
   `golang.org/x/time/rate` token bucket + per-request HTTP timeout). Results
   land in page-indexed slots, so merge order never follows network order.
5. Deterministic merge first: sections by title, adjacent-page continuation
   with an empty display number, answer entries with an explicit `answer_for`
   bound to a unique display number. Only genuinely ambiguous relations call
   the text-only `TextMerger` (bounded input/output, allowlisted
   joins/answer-binds patch, no images). Unresolved ambiguity stays unbound
   with a warning.
6. Server-side ID remap: every model-local asset key becomes `ast_` + UUID in
   the assets map, `asset_id`, and all nested `image_ref`s through one mapping
   table. Dangling, duplicate, unreferenced keys and ID collisions are
   permanent. URIs are `examforge-asset://<id>` (and `…/raw`); the model never
   decides IDs, URIs, keys or digests.
7. Full `ir.Validator.ValidateExamDTO` plus slice-3 bbox constraints (required
   `source_bbox`, finite ordered corners in [0,1], page within the rasterized
   document). Up to `MaxRepairRounds` (default 2, hard ceiling 4) bounded
   repair rounds; every repaired candidate is checked against a protected
   snapshot (asset set, IDs, no dropped bbox) before revalidation. Repair
   requests carry the candidate JSON and truncated error paths, never images.
8. Only after validation passes: `raster.CropPNG` on the high-resolution page
   and `clean.HSVCleaner`. Crop failure is permanent (error names asset and
   page); cleaner failure falls back to `raw_cropped` with a warning and no
   clean payload. Identical clean/raw bytes are never stored as `cleaned`.
9. Real `aspect_ratio`/`suggested_width_mm`/`status` backfill, then a final
   read-only full validation. A failure there is a permanent consistency
   defect, never another LLM round.

Configuration (`cloud.Config`): live endpoint only from
`EXAMFORGE_VLM_BASE_URL`/`EXAMFORGE_VLM_API_KEY`/`EXAMFORGE_VLM_MODEL`
(`cloud.EnvConfig`); the key is never a flag, constant, fixture, log field or
error text. The default HTTP client has `Proxy: nil`. Independent limits:
page/merge/repair/raster timeouts, page concurrency and rate, request/response
byte caps, output token cap, merge input/output caps, and
`TotalParseTimeout`. `TotalParseTimeout + WorkerReserve` must stay strictly
below `JobTimeout` when configured; the budget check refuses external calls
once the remaining budget cannot cover the worker reserve (transient). HTTP
classification: 400/401/403 and protocol/oversize defects permanent; 429/5xx/
network/timeout transient; parent cancellation stays a context error.
`Diagnostics` reports pages, per-stage milliseconds, token usage, repair
rounds, merge calls and warning counts — never IR, base64 or secrets.
`Extractor`/`Merger` config seams let tests inject protocol fakes.

### Rasterization and cropping (`internal/vision/raster/`)

- `Rasterizer` interface; `MuPDFRasterizer` runs `pdfinfo` + `mutool draw`
  through `os/exec` `CommandContext` with per-command `context.WithTimeout`,
  a fixed minimal child environment, bounded stdout/stderr capture, and a
  private temp dir removed on success, failure, cancel and timeout.
- Page count comes only from the uniquely anchored `pdfinfo` `Pages:` line;
  missing/duplicate/non-positive counts fail, and `MaxPages` is checked before
  any `mutool draw` runs. `mutool draw` uses fixed `-F png -r <dpi> -o
  page-%d.png` arguments; output file count, per-page bytes, per-page pixels
  and total decoded pixels are all bounded.
- A process-wide semaphore (`SetProcessLimit`) bounds concurrent subprocesses
  across all rasterizer instances; the injectable `CommandFactory` seam goes
  through the identical semaphore/timeout/cleanup path (fake-shell-script
  tests prove it).
- `CropPNG(page, [x1,y1,x2,y2], dpi, CropConfig)`: normalized corner
  coordinates, NaN/Inf/order/range rejection, outward rounding, padding in mm
  converted through the DPI, clamping to image bounds, empty-rectangle
  rejection, pixel ceiling. Returns PNG + SHA-256 + measured geometry;
  `SuggestedWidthMM` uses 25.4 mm/in capped by `MaxContentWidthMM`.
- `internal/vision/testpdf/` builds small deterministic valid multi-page PDFs
  for physical tests against real `mutool`/`pdfinfo`.

### Cleaning (`internal/vision/clean/`)

- `ImageCleaner` interface; `HSVCleaner` is pure Go `image/color` pixel
  processing (no CGO/OpenCV). `HSVConfig` is validated at construction
  (finite, in-range, non-contradictory thresholds).
- Classification priority: dark protection (print, thin/dashed black lines)
  first; then saturated red/blue annotations (red hue range wraps through 0°);
  then low-saturation high-brightness gray watermark. Everything else is
  kept; classified pixels become white; transparent input composites over
  white. Input bytes/pixels bounded; ctx checked per row.
- Failure returns an error; the caller records a warning and keeps
  `raw_cropped`. The cleaner never fills clean bytes with the raw image.
- Physical falsification tests assert ≥95% removal of red/blue/watermark
  pixels and ≥99% retention of black print/lines on a synthetic masked
  fixture, and that the output is not all white.

### Validation and uploads (worker side)

The worker validates the candidate through `ir.Validator.ValidateExamDTO`
before any upload. Each payload variant is type-checked
(`validateAssetBytes`), size-bounded, and hashed server-side. Status selects
the upload shape: `raw_cropped` uploads one object under
`storage.RawAssetKey` (main key = raw); `cleaned` uploads two different
objects — clean under `storage.CleanAssetKey` as the main key, raw under
`RawAssetKey` as the companion — with independent digests
(`store.VerifiedAsset{SHA256, RawSHA256}`). `vectorized` is rejected. Only
uploaded, digest-verified objects reach the persistence transaction.

## Boundaries

No OCR/layout-analysis vendor, local GPU worker, LaMa/Real-ESRGAN, DOCX
export or HTTP API exists. Future OCR/layout services implement separate
adapters, not the OpenAI-compatible transport. Real paid VLM calls happen
only through the operator CLI (`cmd/exampipe -parser cloud`) with explicit
`EXAMFORGE_VLM_*` environment; the offline gate and the integration gate use
localhost `httptest` fakes exclusively. There is no cross-attempt request
deduplication: a River retry re-calls page APIs and may bill again; the
diagnostics record usage so operators can measure budgets.