#!/usr/bin/env bash
# test_check_doc_freshness.sh — self-contained gate test for
# check_doc_freshness.sh.
#
# Builds a throwaway git repository containing the four gated reference
# doc paths and verifies three properties of the freshness gate:
#   1. Freshly-stamped docs PASS (exit 0).
#   2. A content-only edit that does NOT re-stamp the footer FAILS
#      (exit non-zero) with the re-stamp error message.
#   3. Re-stamping the footer in a follow-up commit PASSES again.
#
# This is the regression gate for the strict re-stamp rule: property 2
# fails on the un-extended script and passes once the strict check is
# present.
#
# Run directly from the repo root:
#   bash scripts/test_check_doc_freshness.sh

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$REPO/scripts/check_doc_freshness.sh"

command -v git >/dev/null 2>&1 || { echo "SKIP: git not available"; exit 0; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

DOCS=(persistence cypher bolt test-battery)

(
  cd "$tmp"
  git init -q
  git config user.email tester@example.invalid
  git config user.name tester
  git commit --allow-empty -qm "base"
  base_short="$(git rev-parse --short HEAD)"

  mkdir docs
  for d in "${DOCS[@]}"; do
    {
      printf '# %s\n\nbody text\n\n---\n\n' "$d"
      printf '*Last reviewed: 2026-06-12 against commit `%s`. footer.*\n' "$base_short"
    } > "docs/$d.md"
  done
  git add docs
  git commit -qm "add gated docs with fresh footers"
)

run_gate() { ( cd "$tmp" && bash "$SCRIPT" ); }

# 1) Fresh docs must pass.
if run_gate >"$tmp/out1" 2>&1; then
  echo "PASS [1/3]: fresh footers accepted"
else
  echo "FAIL [1/3]: fresh footers were rejected"; cat "$tmp/out1"; exit 1
fi

# 2) Content-only edit without a re-stamp must fail.
(
  cd "$tmp"
  printf 'an added paragraph without re-stamping\n' >> docs/bolt.md
  git commit -qam "edit bolt body, leave footer stale"
)
if run_gate >"$tmp/out2" 2>&1; then
  echo "FAIL [2/3]: stale-after-edit doc was accepted"; cat "$tmp/out2"; exit 1
else
  if grep -q "did not update the 'Last reviewed:" "$tmp/out2"; then
    echo "PASS [2/3]: edit-without-restamp rejected with the expected message"
  else
    echo "FAIL [2/3]: rejected, but not with the strict re-stamp message"; cat "$tmp/out2"; exit 1
  fi
fi

# 3) Re-stamping the footer in a follow-up commit must pass again.
(
  cd "$tmp"
  new_short="$(git rev-parse --short HEAD)"
  # Rewrite docs/bolt.md keeping the body edit and bumping the footer SHA
  # (touching the footer line).
  {
    printf '# bolt\n\nbody text\n\nan added paragraph without re-stamping\n\n---\n\n'
    printf '*Last reviewed: 2026-06-12 against commit `%s`. footer.*\n' "$new_short"
  } > docs/bolt.md
  git commit -qam "re-stamp bolt footer"
)
if run_gate >"$tmp/out3" 2>&1; then
  echo "PASS [3/3]: re-stamped footer accepted"
else
  echo "FAIL [3/3]: re-stamped footer was rejected"; cat "$tmp/out3"; exit 1
fi

echo "ALL CASES PASSED"
