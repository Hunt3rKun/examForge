# Vision and parser — current-state contract

**Status:待实现。** No OCR, image cleanup, AI client, or parsing implementation is included in Stage 0.

Future extension points are `ParserAdapter` (convert source pages into structured content candidates) and `ImageCleaner` (normalize/crop/clean image assets). Keep both behind interfaces and provide offline mocks. Source-page rendering/cropping must use bounded `os/exec` wrappers (for example `mutool` or `pdftoppm`), never CGO. Future work should update this contract only when source paths and behavior are implemented.
