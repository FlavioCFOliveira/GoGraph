#!/usr/bin/env bash
# check_doc_freshness.sh — enforce the "Last reviewed: <date> against
# commit <sha>" footer on the key reference docs, and ensure that footer
# is actually re-stamped whenever the doc is changed.
#
# Fails the build when, for any gated doc:
#   * the footer is missing, or
#   * the footer's commit SHA is not reachable from HEAD (typo /
#     rebase noise), or
#   * the footer's commit SHA is older than $MAX_COMMITS_BEHIND commits
#     behind HEAD (default 200) — catches an obviously abandoned stamp,
#     or
#   * the most recent commit that modified the doc did NOT also modify
#     the footer line.
#
# The last rule is the one that gives the footer its promised teeth.
# Before it existed, a doc could be substantively rewritten while the
# footer kept an old (still-reachable, still-within-threshold) SHA and
# the gate stayed green — exactly what happened to docs/bolt.md, which
# was rewritten on 2026-06-04 with a 2026-05-30 stamp left in place.
#
# Re-stamping is unambiguous and chicken-and-egg-free: a correct change
# edits the footer line in the SAME commit that edits the body, and a
# pure re-stamp (a footer-only commit) trivially satisfies the rule
# because that commit, by definition, touches the footer line.
#
# Invoke from the repo root:
#   bash scripts/check_doc_freshness.sh
#
# Override the staleness threshold with MAX_COMMITS_BEHIND=<n>.
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
  "docs/test-battery.md"
)

MAX_COMMITS_BEHIND="${MAX_COMMITS_BEHIND:-200}"

# footer_touched_in <commit> <doc> — succeeds when <commit>'s diff for
# <doc> adds or removes a line carrying the "Last reviewed:" footer
# marker. The +++/--- diff path headers are excluded so a file path that
# happens to contain the marker cannot produce a false positive.
footer_touched_in() {
  local commit="$1" doc="$2"
  git show --format= --unified=0 "$commit" -- "$doc" 2>/dev/null \
    | grep -E '^[+-]' \
    | grep -vE '^(\+\+\+|---) ' \
    | grep -q 'Last reviewed:'
}

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

  # Re-stamp enforcement: the most recent commit that changed the doc
  # must also have changed the footer line. This closes the gap where a
  # doc is edited but its footer is left stale.
  last_commit="$(git log -1 --format=%H -- "$doc" 2>/dev/null || true)"
  if [[ -n "$last_commit" ]] && ! footer_touched_in "$last_commit" "$doc"; then
    short="$(git rev-parse --short "$last_commit")"
    echo "::error file=$doc::doc-freshness: $doc was last changed in commit $short, which did not update the 'Last reviewed: ... against commit <sha>' footer. Re-read the doc against current code and re-stamp the footer in the same commit."
    fail=1
    continue
  fi

  echo "doc-freshness OK: $doc ($ahead commits behind HEAD; footer re-stamped in last change ${last_commit:0:7})"
done

if (( fail != 0 )); then
  exit 1
fi
