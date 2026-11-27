#!/usr/bin/env bash
# ExamForge verification gate. Runs fully offline: every Typst compile uses
# the vendored MiTeX package cache, never the network or the user package cache.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

fail() { printf 'verify: %s\n' "$1" >&2; exit 1; }

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

render_one() {
  local mode="$1"
  local out="$tmpdir/${mode}.pdf"
  go run ./cmd/examrender \
    -ir testdata/golden/ir/sample.json \
    -layout "testdata/golden/layout/${mode}.json" \
    -assets testdata/golden/assets \
    -out "$out"
  test -s "$out" || fail "empty output: $out"
  head -c 5 "$out" | grep -q '%PDF-' || fail "output is not a PDF: $out"
  pdfinfo "$out" >/dev/null || fail "pdfinfo cannot read: $out"
}

# Offline stage: re-executed inside a network namespace when unshare works.
if [[ "${1:-}" == "--offline-only" ]]; then
  render_one student
  render_one teacher
  printf 'verify: offline renders passed\n'
  exit 0
fi

# ── Toolchain ────────────────────────────────────────────────────────────────
for tool in typst prlimit mutool pdftotext pdfinfo staticcheck sqlc; do
  command -v "$tool" >/dev/null 2>&1 || fail "required executable not found: $tool"
done
typst --version 2>/dev/null | grep -q '^typst 0\.15\.' || fail "Typst 0.15.x is required"
[[ "$(sqlc version)" == "v1.31.1" ]] || fail "sqlc v1.31.1 is required, found $(sqlc version)"
typst fonts --font-path third_party/fonts --ignore-system-fonts 2>/dev/null |
  grep -qx 'Noto Sans CJK SC' || fail "vendored font not found: Noto Sans CJK SC"

# ── Go checks ────────────────────────────────────────────────────────────────
unformatted="$(gofmt -l internal cmd)"
[[ -z "$unformatted" ]] || fail "gofmt: not formatted: $unformatted"
go vet ./...
staticcheck ./...
go test -race -count=1 ./...
go run ./cmd/ircheck -schema schemas/exam-ir-v1.json -dir testdata/golden/ir
go run ./cmd/ircheck -schema schemas/layout-spec-v1.json -dir testdata/golden/layout
go run ./cmd/ircheck -schema schemas/exam-ir-v1.json -dir testdata/render/ir
go run ./cmd/ircheck -schema schemas/layout-spec-v1.json -dir testdata/render/layouts

# ── sqlc drift gate ─────────────────────────────────────────────────────────
# Generated code is committed. Regenerating must change nothing; a missing
# config or query file fails here instead of shipping a stale store.
[[ -f sqlc.yaml ]] || fail "sqlc.yaml is missing"
[[ -d internal/store/queries ]] || fail "internal/store/queries is missing"
[[ -d internal/store/sqlcgen ]] || fail "internal/store/sqlcgen is missing"
sqlc diff || fail "sqlc generated code is stale; run sqlc generate"

# ── CLI resource check (schemas, template, vendored package, assets) ─────────
go run ./cmd/examrender -check \
  -ir testdata/golden/ir/sample.json \
  -layout testdata/golden/layout/student.json \
  -assets testdata/golden/assets

# ── Representative renders, online and with the network cut ──────────────────
render_one student
cp "$tmpdir/student.pdf" "$tmpdir/student-first.pdf"
render_one student
cmp -s "$tmpdir/student-first.pdf" "$tmpdir/student.pdf" || fail "identical inputs produced different PDF bytes"
render_one teacher

if command -v unshare >/dev/null 2>&1 && unshare -r -n true 2>/dev/null; then
  printf 'verify: rendering with the network cut (unshare -r -n)\n'
  unshare -r -n bash "$0" --offline-only || fail "offline render failed"
else
  printf 'verify: unshare unavailable; offline render enforced by the blackholed proxy environment\n'
fi

printf 'verify: all checks passed\n'