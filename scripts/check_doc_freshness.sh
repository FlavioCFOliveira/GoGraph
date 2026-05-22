#!/usr/bin/env bash
# check_doc_freshness.sh — sanity-check the 'Last reviewed: <date>
# against commit <sha>' footer in the key reference docs.
#
# Fails the build when:
#   * any tracked doc in the list below is missing the footer, or
#   * the footer's commit SHA is not reachable from HEAD (typo /
#     rebase noise), or
#   * the footer's commit SHA is older than $MAX_COMMITS_BEHIND
#     commits behind HEAD (default 200). The threshold is high
#     enough that legitimate doc-only updates do not need to
#     re-stamp the footer every release; it catches obvious neglect
#     such as a year-old commit reference.
#
# Invoke from the repo root:
#   bash scripts/check_doc_freshness.sh
#
# The script is intentionally self-contained so it can run from any
# CI worker without project-specific helpers.

set -euo pipefail

# Files to gate. Add new entries when a reference doc joins the
# corpus and is expected to be re-stamped on substantive change.
DOCS=(
  "docs/persistence.md"
  "docs/cypher.md"
  "docs/bolt.md"
)

MAX_COMMITS_BEHIND="${MAX_COMMITS_BEHIND:-200}"

fail=0
for doc in "${DOCS[@]}"; do
  if [[ ! -f "$doc" ]]; then
    echo "::error file=$doc::doc-freshness: $doc not found"
    fail=1
    continue
  fi
  # Extract the SHA from a line like:
  #   *Last reviewed: 2026-05-22 against commit `abcd123`. ...*
  sha="$(grep -oE 'against commit `[0-9a-f]+`' "$doc" | head -n1 | sed -E 's/.*`([0-9a-f]+)`.*/\1/')"
  if [[ -z "$sha" ]]; then
    echo "::error file=$doc::doc-freshness: missing 'Last reviewed: ... against commit <sha>' footer"
    fail=1
    continue
  fi
  if ! git cat-file -e "${sha}^{commit}" 2>/dev/null; then
    echo "::error file=$doc::doc-freshness: footer SHA $sha is not a known commit (typo, force-push, or amend?)"
    fail=1
    continue
  fi
  # Count how many commits HEAD is ahead of $sha. If the doc's SHA
  # is not an ancestor of HEAD, git rev-list returns an error code
  # and we treat that as a freshness failure.
  if ! ahead="$(git rev-list --count "${sha}..HEAD" 2>/dev/null)"; then
    echo "::error file=$doc::doc-freshness: footer SHA $sha is not an ancestor of HEAD"
    fail=1
    continue
  fi
  if (( ahead > MAX_COMMITS_BEHIND )); then
    echo "::error file=$doc::doc-freshness: footer is $ahead commits behind HEAD (threshold $MAX_COMMITS_BEHIND); re-stamp after re-reading the doc against current code"
    fail=1
    continue
  fi
  echo "doc-freshness OK: $doc ($ahead commits behind HEAD)"
done

if (( fail != 0 )); then
  exit 1
fi
