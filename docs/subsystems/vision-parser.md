# Vision and parser — current-state contract

## Implemented (MVP slice 2)

`internal/parser` is the boundary between a source PDF and a candidate Exam
IR. An adapter reads source bytes and returns the candidate document plus the
asset bytes it references; it never writes a database, uploads objects, or
enqueues jobs. The ingest worker validates the output and owns every side
effect.

### Contract

- `Source{JobID, StorageKey, SHA256, Open}`: `Open` returns a bounded,
  context-honoring stream. `ReadSource` reads and verifies it (size bound
  `storage.MaxSourceBytes`, digest match) before parsing.
- `AssetBlob{AssetID, MediaType, Data, SHA256}`: asset bytes plus an optional
  declared digest; a declared digest that does not match the bytes is a
  permanent input failure.
- `ParseResult{Exam, Assets}`: the candidate IR and its asset bytes.
- `ParserAdapter.Parse(ctx, Source) (ParseResult, error)`.
- `parser.Error{Permanent, Code, Err}`: permanent marks input defects that must
  not be retried; anything else is transient. The pipeline maps permanent
  parser errors to the `parser_<code>` failure code.

### Implemented adapter

`MockParserAdapter` replays a golden Exam IR and its asset files for any valid
PDF source, keeping the real contract (bounded hashed read, candidate IR,
assets with declared digests). The asset manifest, never the IR `uri`, decides
which file an asset ID names. Fault hooks for tests: `FromGolden`,
`InvalidTimes` (schema-invalid candidate → permanent failure + cancel),
`FailTimes` (transient failure → River retry), `DuplicateQuestions`, `Delay`,
`Mutate`; `Calls`, `InjectInvalid`, `InjectFailures` adjust state at runtime.

### Validation and uploads

The worker validates the candidate through `ir.Validator.ValidateExamDTO`
before any upload. Asset bytes are type-checked (`validateAssetBytes`: PNG/
JPEG/GIF header decode and 40M-pixel bound, WebP magic, SVG signature), hashed,
and uploaded through immutable puts under deterministic keys
(`storage.RawAssetKey`). Only uploaded, digest-verified objects reach the
persistence transaction as `store.VerifiedAsset`.

## Boundaries

No OCR, image cleanup, AI/VLM client, or PDF rasterization exists yet. Future
work adds `ParserAdapter` implementations and an `ImageCleaner` behind these
interfaces with offline mocks; source-page rasterization must use bounded
`os/exec` wrappers (`mutool`, `pdftoppm`), never CGO. Update this contract only
when those paths exist.
