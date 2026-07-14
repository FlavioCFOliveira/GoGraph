#!/usr/bin/env bash
# pre-release.sh — standalone correctness gate (no coverage).
#
# This is a convenience gate for a quick vet/build/-race/lint sweep WITHOUT the
# slow coverage-instrumented run. It is NOT the canonical release gate: that is
# `make release-preflight`, which runs `make ci` (vet/build/test-short[-race]/
# lint/cover-gate) exactly once plus release-accuracy and the headline bench.
#
# Usage:
#   bash scripts/pre-release.sh [version]
#
# Steps:
#   1. go vet ./...
#   2. go build ./...
#   3. go test -race ./... (includes bench, cypher, tck, bolt)
#   4. golangci-lint run ./...
#   5. Print PASS / FAIL summary
#
# openCypher TCK conformance is NOT re-checked here: the =100% execution
# baseline (cypher/tck TestTCKExecution, const tckExecutionBaseline) already
# runs inside step 3's `go test -race ./cypher/tck/...`, so any regression fails
# this gate without a separate — and strictly weaker — pass-rate re-run. The
# former `TestTCKReport overall-rate >= 90%` step was removed because it was
# redundant with that baseline and rewrote docs/tck/parser-report.md as a side
# effect, which could dirty the working tree ahead of `make release`'s
# clean-tree check.
#
# The soak step (SOAK_FULL=1) is excluded from the automated gate because it
# takes 4+ hours; run it manually before a major release.
#
# -timeout=30m on the race run: go test's default per-package timeout is 10
# minutes. internal/sim (the DST/simulation harness) legitimately takes
# ~400-420s under -race on a fast local machine (Apple M4), but GitHub-hosted
# CI runners are slower and have been observed to exceed the 10-minute
# default, killing the whole run with "panic: test timed out after 10m0s" —
# a runner-speed artifact, not a hang or a real regression (verified: the
# identical failure occurs on unrelated commits with no changes to
# internal/sim). 30m gives roughly 4x headroom over the local measurement,
# mirroring the precedent set by raising the goreleaser job's own timeout
# 20m->45m for the same class of issue (see .github/workflows/release.yml).
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
run_step "go test -race ./..."         go test -race -timeout=30m ./...
run_step "golangci-lint run ./..."     golangci-lint run ./...

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
