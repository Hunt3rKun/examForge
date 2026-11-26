# third_party — vendored resources

## typst-packages/preview/mitex/0.2.7

MiTeX, vendored for offline Typst rendering. This directory is a Typst package
cache root: `typst --package-cache-path third_party/typst-packages` resolves
`@preview/mitex:0.2.7` from it without any network access.

- **Package:** `@preview/mitex` version `0.2.7`
- **Source archive:** <https://packages.typst.org/preview/mitex-0.2.7.tar.gz>
- **Archive SHA-256:** `0159e214845e49cbdc332d9d572da112dae5ad248072e0a7680d38c8307c2e15`
- **License:** Apache-2.0 (full text in `preview/mitex/0.2.7/LICENSE`)
- **Verified against:** Typst 0.15.1 (inline `mi()` and display `mitex()`,
  including the `.wasm` plugin), offline with the network cut.

The per-file SHA-256 manifest is pinned in Go at
`internal/render/vendored.go` (`mitexFiles`). The renderer verifies every file
before each compile and refuses missing, tampered or extra files; the template
may import only this exact package version. Do not edit files here: update the
version, the archive hash and the Go manifest together.