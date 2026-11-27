#!/usr/bin/env bash
# ExamForge explicit integration gate (slice 2). It connects to a real
# PostgreSQL and an S3-protocol service (SeaweedFS), so it is never part of the
# default offline gate; run it by hand when those services are available.
#
# Every connection value comes from the environment. Nothing here supplies a
# default endpoint, host, bucket or credential: a missing or malformed value
# fails before any test runs, and the values themselves are never printed.
#
# Usage:
#   ./scripts/verify-integration.sh                     # env already exported
#   ./scripts/verify-integration.sh --env-file FILE     # source FILE first
#   ./scripts/verify-integration.sh --env-file FILE --check-only
#       # validate the contract and probe PostgreSQL + S3 only; no test suite
#
# Required:
#   EXAMFORGE_TEST_DATABASE_URL          postgres:// or postgresql:// URL
#   EXAMFORGE_TEST_S3_ENDPOINT           http:// or https:// base URL
#   EXAMFORGE_TEST_S3_BUCKET             existing bucket name
#   EXAMFORGE_TEST_S3_REGION             region string
#   EXAMFORGE_TEST_S3_ACCESS_KEY_ID      access key
#   EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY  secret key
#   EXAMFORGE_TEST_S3_FORCE_PATH_STYLE   must be exactly "true"
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

fail() { printf 'verify-integration: %s\n' "$1" >&2; exit 1; }

env_file=""
check_only=0
while (( $# > 0 )); do
  case "$1" in
    --env-file)
      (( $# >= 2 )) || fail "--env-file needs a path"
      env_file="$2"; shift 2 ;;
    --env-file=*) env_file="${1#*=}"; shift ;;
    --check-only) check_only=1; shift ;;
    -h|--help) sed -n '2,23p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

if [[ -n "$env_file" ]]; then
  [[ -f "$env_file" ]] || fail "env file not found: $env_file"
  # The file holds a database password and an S3 secret. Refuse a file other
  # accounts can read rather than silently using it.
  mode="$(stat -c '%a' "$env_file" 2>/dev/null || stat -f '%Lp' "$env_file")"
  (( (8#$mode & 8#077) == 0 )) ||
    fail "$env_file is readable by group/other (mode $mode); run: chmod 600 $env_file"
  # shellcheck disable=SC1090
  source "$env_file"
fi

required=(EXAMFORGE_TEST_DATABASE_URL EXAMFORGE_TEST_S3_ENDPOINT EXAMFORGE_TEST_S3_BUCKET
  EXAMFORGE_TEST_S3_REGION EXAMFORGE_TEST_S3_ACCESS_KEY_ID EXAMFORGE_TEST_S3_SECRET_ACCESS_KEY
  EXAMFORGE_TEST_S3_FORCE_PATH_STYLE)
missing=()
for name in "${required[@]}"; do
  [[ -n "${!name:-}" ]] || missing+=("$name")
done
if (( ${#missing[@]} > 0 )); then
  fail "missing environment variables: ${missing[*]}"
fi

# Path-style addressing is mandatory; virtual-hosted bucket URLs are rejected.
[[ "$EXAMFORGE_TEST_S3_FORCE_PATH_STYLE" == "true" ]] ||
  fail "EXAMFORGE_TEST_S3_FORCE_PATH_STYLE must be exactly 'true'"

case "$EXAMFORGE_TEST_DATABASE_URL" in
  postgres://*|postgresql://*) ;;
  *) fail "EXAMFORGE_TEST_DATABASE_URL must start with postgres:// or postgresql://" ;;
esac
case "$EXAMFORGE_TEST_S3_ENDPOINT" in
  http://*|https://*) ;;
  *) fail "EXAMFORGE_TEST_S3_ENDPOINT must start with http:// or https://" ;;
esac
[[ "$EXAMFORGE_TEST_S3_BUCKET" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] ||
  fail "EXAMFORGE_TEST_S3_BUCKET is not a valid bucket name"
[[ "$EXAMFORGE_TEST_S3_REGION" =~ ^[A-Za-z0-9-]+$ ]] ||
  fail "EXAMFORGE_TEST_S3_REGION is not a valid region"

for tool in go typst; do
  command -v "$tool" >/dev/null 2>&1 || fail "required executable not found: $tool"
done

# Masked summary: host and bucket only, never a password or secret.
db_after_scheme="${EXAMFORGE_TEST_DATABASE_URL#*://}"
db_hostpart="${db_after_scheme##*@}"
printf 'verify-integration: postgres=%s s3=%s bucket=%s\n' \
  "${db_hostpart%%/*}" "${EXAMFORGE_TEST_S3_ENDPOINT}" "$EXAMFORGE_TEST_S3_BUCKET"

if (( check_only == 1 )); then
  # Proves connectivity, credentials, CREATE privilege and both migration sets
  # without running the full suite. No -race here: the probe needs no compiler.
  go test -tags=integration -count=1 -timeout=3m -run '^TestIntegrationPreflight$' ./internal/pipeline/ ||
    fail "preflight failed: see the test output above"
  printf 'verify-integration: preflight passed\n'
  exit 0
fi

# Compile-check every tagged package before the slow run so a type error in
# integration-only code fails fast.
go vet -tags=integration ./...

# The race detector needs cgo at test time only; production builds stay
# CGO-free and ./scripts/verify.sh enforces that.
go test -tags=integration -race -count=1 -timeout=15m ./...

printf 'verify-integration: all integration checks passed\n'
