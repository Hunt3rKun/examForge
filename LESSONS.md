# Lessons

Dynamic engineering retrospective log. Record each lesson using this fixed format:

```markdown
### YYYY-MM-DD — <short lesson title>
- **Rule summary:** <actionable rule>
- **Pitfall and commit:** <why it failed; commit hash or `uncommitted`>
- **Reproduction and verification:** <steps/commands and expected result>
- **Seen N×:** <count> (dates: YYYY-MM-DD, ...)
```

When the same class of lesson appears on three different dates, propose promoting it into `docs/code-standards.md` and link the permanent rule here. Do not inflate counts for repeated observations on one date.

### 2026-11-26 — Injection seams must be honored by the constructor, not just declared
- **Rule summary:** When a config struct exposes an injection seam (e.g. `CommandFactory`), the constructor must actually use it; add a fake-injection test that fails if the seam is ignored, and prefer env-independent fakes (generated shell scripts) because the production path may overwrite `cmd.Env` after the factory returns.
- **Pitfall and commit:** `NewTypstRenderer` declared `cfg.CommandFactory` but always built its own `exec.CommandContext` wrapper, so process tests silently exercised real typst; a test-binary re-exec fake also failed because the renderer assigns `cmd.Env` after the factory returns. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/render/ -run 'TestRenderFakeSuccess|TestRenderSemaphoreBound'` — fake-script tests observe start/end activity files and pass only when the injected factory is used.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Typst content position prints arrays as code; visual inspection catches it
- **Rule summary:** In Typst templates, an array placed in content position renders as its code representation — always `.join()` mapped content. Assert geometry in tests AND rasterize/inspect at least one PDF per template change; text-only assertions missed both the array-as-code bug and justification stretching text past a narrowed float column.
- **Pitfall and commit:** `block.blocks.map(render-block)` in the float grid cell printed `par(body: [...])` literally; after `.join()`, `justify: true` stretched beside-image CJK text ~5pt past the narrowed column. Both were invisible to pdftotext-based tests until the PNG was inspected. `uncommitted`
- **Reproduction and verification:** Render `testdata/render/ir/pages.json` with `pages-nobinding.json` via `go run ./cmd/examrender`, `pdftoppm -png -r 80 out.pdf probe`, inspect: beside-image paragraphs stay inside the narrowed column and no code text appears; `TestRenderFloatSideBySide` asserts word x-bounds.
- **Seen 1×:** (dates: 2026-11-26)

### 2026-11-26 — Geometry probes must measure across the feature, not beside it
- **Rule summary:** When asserting a layout feature (reserve space, indent, gutter) from PDF word coordinates, pick probe words that straddle the feature's actual render position; verify probe placement against a fresh render before trusting a failing/passing assertion. Stale PDF dumps from earlier test runs mislead diagnosis.
- **Pitfall and commit:** The student/teacher reserve test measured 填空→样例 inside one stem, but `reserve_space_mm` renders after the whole stem, so both modes gave identical 55.2pt gaps; a distance-to-page-bottom probe also failed because trailing whitespace is unobservable without following content. Fixed by adding a probe question pair (Q3 reserve 30mm, Q4 follower) and asserting the stem-to-stem gap difference. `uncommitted`
- **Reproduction and verification:** `go test -race -count=1 ./internal/render/ -run TestRenderStudentTeacherDifference` passes; student gap − teacher gap ≥ 30mm − 2pt.
- **Seen 1×:** (dates: 2026-11-26)
