#!/bin/bash
# godocdiff.sh — authoritative exported-surface diff between two trees, using the
# real Go parser via `go doc -all` rather than a regex over source. Doc comments are
# stripped so only declaration lines survive; struct FIELDS therefore appear too,
# which a top-level-declaration scan cannot see.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad
PKGS="$(cd "$SP/bench.noindex/B" && go list ./... \
   | sed 's#^github.com/FlavioCFOliveira/GoGraph##; s#^/##' \
   | grep -vE '(^|/)(internal|examples|cmd)(/|$)' \
   | grep -vE '^(cypher/parser/gen|cypher/tck)' \
   | grep -vE '^bench/(soak|comparison|audit352|cyclicjoin|mvccwrite|mtaudit|cypher_ldbc|dimacs9_cli)' )"
for arm in A B; do
  : > "$SP/work/surface_$arm.txt"
  for p in $PKGS; do
    d="$SP/bench.noindex/$arm/${p:-.}"
    [ -d "$d" ] || continue
    ( cd "$d" && go doc -all -C . . 2>/dev/null ) \
      | grep -E '^(func |type |    [A-Z][A-Za-z0-9_]* )|^(const|var)' \
      | sed "s#^#${p:-root}: #" >> "$SP/work/surface_$arm.txt"
  done
  sort -u -o "$SP/work/surface_$arm.txt" "$SP/work/surface_$arm.txt"
done
wc -l "$SP/work/surface_A.txt" "$SP/work/surface_B.txt"
