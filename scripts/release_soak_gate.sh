#!/usr/bin/env bash
# release_soak_gate.sh — pre-release gate that requires a GREEN soak
# workflow run for the exact commit being released.
#
# CLAUDE.md mandates a multi-hour mixed-workload soak before any
# release ("Soak test before any release"). The v0.2.0 validation
# evidence shipped only the 60-second soak-smoke; the one multi-hour
# soak on record ran two days after the release and failed. This gate
# closes that gap: `make release-preflight` calls it, and it fails the
# release unless GitHub Actions records a successful run of the soak
# workflow for the release commit.
#
# It resolves the release commit as the commit the VERSION tag points to
# (falling back to HEAD), then asks the GitHub CLI for a successful run
# of the soak workflow on that commit.
#
# Environment:
#   VERSION          the release tag (e.g. v0.2.0); resolved to its commit
#   RELEASE_SHA      explicit commit SHA override (skips git resolution)
#   GH_BIN           GitHub CLI binary (default: gh) — overridable for tests
#   SOAK_WORKFLOW    workflow file name (default: soak.yml)
#
# Run directly:
#   VERSION=v0.2.0 bash scripts/release_soak_gate.sh

set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
SOAK_WORKFLOW="${SOAK_WORKFLOW:-soak.yml}"
VERSION="${VERSION:-}"

sha="${RELEASE_SHA:-}"
if [[ -z "$sha" ]]; then
  if [[ -n "$VERSION" ]] && git rev-parse "$VERSION" >/dev/null 2>&1; then
    sha="$(git rev-parse "${VERSION}^{commit}")"
  else
    sha="$(git rev-parse HEAD)"
  fi
fi

if ! command -v "$GH_BIN" >/dev/null 2>&1; then
  echo "release-soak-gate: '$GH_BIN' CLI not found. Install the GitHub CLI and authenticate (gh auth login) so the gate can confirm a green soak run for release commit $sha."
  exit 1
fi

# `gh run list ... --json databaseId --jq length` prints the number of
# matching runs. A query failure (unauthenticated, network, API change)
# yields empty output, which we treat as "cannot verify" → fail closed.
count="$("$GH_BIN" run list --workflow="$SOAK_WORKFLOW" --commit "$sha" --status success --json databaseId --jq 'length' 2>/dev/null || true)"
if ! [[ "$count" =~ ^[0-9]+$ ]]; then
  echo "release-soak-gate: could not query GitHub Actions for '$SOAK_WORKFLOW' runs on $sha (is '$GH_BIN' authenticated? run 'gh auth status'). Failing closed."
  exit 1
fi

if (( count < 1 )); then
  echo "release-soak-gate: no green '$SOAK_WORKFLOW' run found for release commit $sha."
  echo "  The multi-hour soak is a mandatory pre-release gate (CLAUDE.md: 'Soak test before any release')."
  echo "  Trigger it and wait for success before releasing:"
  echo "      gh workflow run $SOAK_WORKFLOW --ref ${VERSION:-<tag>}"
  exit 1
fi

echo "release-soak-gate: OK — $count green '$SOAK_WORKFLOW' run(s) recorded for $sha"
