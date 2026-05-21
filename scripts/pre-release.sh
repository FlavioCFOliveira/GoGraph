#!/usr/bin/env bash
# pre-release.sh — run the full release validation gate before tagging.
#
# Usage:
#   bash scripts/pre-release.sh [version]
#
# Steps:
#   1. go vet ./...
#   2. go test -race ./... (includes bench, cypher, tck, bolt)
#   3. TCK conformance check (overall-rate >= 90%)
#   4. golangci-lint run ./...
#   5. go build ./...
#   6. Print PASS / FAIL summary
#
# The soak step (SOAK_FULL=1) is excluded from the automated gate because it
# takes 4+ hours; run it manually before a major release.
set -euo pipefail

VERSION="${1:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

PASS=0
FAIL=0

run_step() {
  local label="$1"; shift
  printf "  %-50s " "$label..."
  if "$@" > /tmp/pre-release-step.log 2>&1; then
    echo "OK"
    PASS=$((PASS + 1))
  else
    echo "FAIL"
    cat /tmp/pre-release-step.log
    FAIL=$((FAIL + 1))
  fi
}

echo ""
echo "=== GoGraph pre-release gate${VERSION:+ for $VERSION} ==="
echo ""

run_step "go vet ./..."                go vet ./...
run_step "go build ./..."              go build ./...
run_step "go test -race ./..."         go test -race ./...
run_step "golangci-lint run ./..."     golangci-lint run ./...

# TCK conformance gate
printf "  %-50s " "TCK overall-rate >= 90%..."
RATE=$(go test -run=TestTCKReport ./cypher/tck/... 2>&1 | grep -oE 'overall-rate=[0-9.]+' | cut -d= -f2)
if [ -z "$RATE" ]; then
  echo "FAIL (could not parse rate)"
  FAIL=$((FAIL + 1))
elif python3 -c "import sys; sys.exit(0 if float('$RATE') >= 90.0 else 1)" 2>/dev/null; then
  echo "OK (${RATE}%)"
  PASS=$((PASS + 1))
else
  echo "FAIL (${RATE}% < 90%)"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Summary: $PASS passed, $FAIL failed ==="
echo ""

if [ "$FAIL" -gt 0 ]; then
  echo "Pre-release gate FAILED. Fix the issues above before tagging."
  exit 1
fi

echo "Pre-release gate PASSED."
if [ -n "$VERSION" ]; then
  echo ""
  echo "To cut the release tag:"
  echo "  git tag -a $VERSION -m 'Release $VERSION — see release-notes/$VERSION.md and CHANGELOG.md'"
  echo "  git push origin $VERSION"
fi
