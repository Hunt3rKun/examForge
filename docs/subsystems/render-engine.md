# Render engine — current-state contract

## Implemented in Stage 0

- `templates/exam.typ` is a minimal smoke template taking separate `data` (Exam IR JSON) and `layout` (Layout Spec JSON) inputs. It is not a production renderer.
- `testdata/golden/assets/sample_fig.png` is a placeholder image fixture used to exercise image loading.

**Hard boundary:** inline `Markdown / $LaTeX$ / {{blank:N}}` parsing belongs to Go `internal/ir/inline` (planned in MVP slice 1). Typst templates own layout only and must not contain complex regex parsers. Stage 0 deliberately does not implement this parser.

The renderer contract is `Render(ExamDocumentIR, LayoutSpec) -> Artifact`; do not move layout policy into scattered command-line switches. Future expansion is through a common `Renderer` interface with `TypstRenderer`, `DocxRenderer`, and fallback `LaTeXRenderer`. Any external process must use `context.WithTimeout` and clean temporary files.
