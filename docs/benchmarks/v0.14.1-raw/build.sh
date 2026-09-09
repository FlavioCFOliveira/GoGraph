#!/bin/bash
# build.sh — compile every block's test binary once per arm, BEFORE any timing,
# so no compiler CPU ever lands inside a measurement. -trimpath so the tree path
# is not baked in and a binary's sha256 becomes real evidence about the CODE.
# Exit codes are appended to build-exits.log and read from THERE, never from a
# pipeline's status.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad
source "$SP/work/pkgs.sh"
BIN=$SP/bin; mkdir -p "$BIN"
LOG=$SP/work/build-exits.log; : > "$LOG"
echo "GO=$(go version)" >> "$LOG"
echo "START $(date -u +%FT%TZ) load1=$(python3 "$SP/work/load1.py")" >> "$LOG"
for arm in A B B2; do
  tree=$SP/bench.noindex/$arm
  for spec in "${PKGS[@]}"; do
    IFS='#' read -r pkg stem <<< "$spec"
    ( cd "$tree" && go test -trimpath -c -o "$BIN/${stem}_${arm}.test" "./$pkg" ) 2>> "$LOG"
    echo "build arm=$arm pkg=$pkg EXIT=$?" >> "$LOG"
  done
  echo "ARM $arm DONE $(date -u +%FT%TZ) free=$(df -H "$SP" | tail -1 | awk '{print $4}')" >> "$LOG"
done
echo "END $(date -u +%FT%TZ)" >> "$LOG"
echo "BUILD_ALLDONE" >> "$LOG"
