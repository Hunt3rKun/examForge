#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
go vet ./...
go test -race -count=1 ./...
go run ./cmd/ircheck -schema schemas/exam-ir-v1.json -dir testdata/golden/ir
go run ./cmd/ircheck -schema schemas/layout-spec-v1.json -dir testdata/golden/layout

if ! command -v typst >/dev/null 2>&1; then
  printf 'verify: required executable not found: typst\n' >&2
  exit 1
fi
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
root="$PWD"
for mode in student teacher; do
  typst compile --root "$root" --input data=../testdata/golden/ir/sample.json --input layout="../testdata/golden/layout/${mode}.json" "$root/templates/exam.typ" "$tmpdir/${mode}.pdf"
  test -s "$tmpdir/${mode}.pdf"
done
printf 'verify: all checks passed\n'
