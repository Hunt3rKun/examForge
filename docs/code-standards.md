# Code standards

- Format Go with `gofmt`; keep packages cohesive, errors contextual, and APIs small. Prefer standard-library primitives unless a dependency clearly earns its place.
- **No CGO:** reject direct or transitive CGO dependencies. Use bounded `os/exec` subprocesses for tools such as Typst, MuPDF/poppler, and Pandoc.
- Keep `ContentBlock` a flat struct with a discriminating `Type` tag and type-specific optional fields; avoid interface-heavy abstractions in serialized data.
- Bound both CPU and I/O concurrency with explicit semaphores; use `errgroup` to collect/cancel related work. Never launch unbounded goroutines.
- Physical falsification: before fixing a bug, write a reproduction test that reliably fails against the old implementation and passes after the fix.
- Fix reaches every sibling: when fixing a branch/path, inspect and address all equivalent branches and call sites, not only the triggering example.
- Tests that start subprocesses or create temporary directories must register cleanup with `t.Cleanup`; avoid leaking child processes and artifacts on failure.
- Prefer representative real samples and golden fixtures over fabricated data. Clearly label unavoidable placeholders.
- Keep content representation independent from layout; renderer APIs receive both Exam IR and `LayoutSpec` explicitly. Inline parsing belongs in Go, not complex Typst regexes.
