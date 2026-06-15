#!/usr/bin/env bash
# bench_gate.sh — compare two benchstat result files and fail when any
# benchmark regresses beyond the threshold.
#
# Usage:
#   bench_gate.sh <base.txt> <head.txt> [threshold_pct]
#
#   base.txt       raw `go test -bench` output for the baseline revision
#   head.txt       raw `go test -bench` output for the candidate revision
#   threshold_pct  maximum allowed percentage increase (default: 10)
#                  A +10.1% delta on any benchmark triggers failure.
#
# Exit codes:
#   0  no regression above threshold
#   1  one or more benchmarks regressed above threshold
#   2  usage error or benchstat not found
#
# The script uses `benchstat -format csv` to parse the comparison; this
# avoids brittle text-alignment parsing and works with every benchstat
# version that supports the -format flag (golang.org/x/perf >= 2022).
#
# Regression detection logic:
#   - The "vs base" column in CSV is e.g. "+99.90%", "-5.00%", or "~".
#   - Only positive deltas above <threshold_pct> on the sec/op metric
#     trigger failure.  Allocation deltas are reported but do not fail
#     the gate on their own (they are often noisy on CI machines).
#   - The "geomean" row is excluded: it aggregates all benchmarks and a
#     single regression already surfaces on the individual-benchmark row.

set -euo pipefail

BASE="${1:-}"
HEAD="${2:-}"
THRESHOLD="${3:-10}"

if [[ -z "$BASE" || -z "$HEAD" ]]; then
  echo "usage: $(basename "$0") <base.txt> <head.txt> [threshold_pct]" >&2
  exit 2
fi

if [[ ! -f "$BASE" ]]; then
  echo "error: base file not found: $BASE" >&2
  exit 2
fi

if [[ ! -f "$HEAD" ]]; then
  echo "error: head file not found: $HEAD" >&2
  exit 2
fi

BENCHSTAT="$(command -v benchstat 2>/dev/null || echo "$(go env GOPATH)/bin/benchstat" 2>/dev/null || echo "")"
if [[ -z "$BENCHSTAT" || ! -x "$BENCHSTAT" ]]; then
  echo "error: benchstat not found; install with: go install golang.org/x/perf/cmd/benchstat@latest" >&2
  exit 2
fi

# Disappearance guard: a benchmark that is present in the baseline but
# absent from the head run produces NO comparison row, so the threshold
# check below would see zero deltas and pass — silently un-gating every
# future regression in that hot path. Verify the set of benchmark names in
# the head output is a SUPERSET of the baseline's: any baseline benchmark
# missing from head fails the gate (covers both deletion and rename).
#
# Names are taken from the raw `go test -bench` lines (e.g.
# "BenchmarkDijkstra_PostWarmup-8   1000 ...") with the trailing
# "-<GOMAXPROCS>" suffix stripped so the comparison is parallelism-agnostic.
bench_names() {
  grep -oE '^Benchmark[^[:space:]]+' "$1" 2>/dev/null | sed -E 's/-[0-9]+$//' | sort -u
}

MISSING=0
missing_list="$(comm -23 <(bench_names "$BASE") <(bench_names "$HEAD"))"
if [[ -n "$missing_list" ]]; then
  while IFS= read -r bm; do
    [[ -z "$bm" ]] && continue
    echo "DISAPPEARED: ${bm} ran in the baseline but is absent from the head output (renamed or removed?)"
    MISSING=$((MISSING + 1))
  done <<< "$missing_list"
fi

# Produce human-readable diff for the log first.
echo "── benchstat comparison ──"
"$BENCHSTAT" "$BASE" "$HEAD" || true
echo

# Parse CSV output.  benchstat writes footnote markers to stderr when all
# samples are equal; redirect stderr to /dev/null to keep output clean.
CSV_OUT="$("$BENCHSTAT" -format csv "$BASE" "$HEAD" 2>/dev/null)"

# Track state: we are inside the sec/op table when the header row just
# seen contained "sec/op".  Allocation tables (B/op, allocs/op) are
# logged but do not fail the gate.
REGRESSIONS=0
current_unit=""   # which benchstat metric table we are inside (sec/op, B/op, allocs/op, B/s)

while IFS=',' read -r name _base_val _base_ci _head_val _head_ci vsbase _pval; do
  # Detect a metric-table header row (empty name, "vs base" in the delta
  # column) and capture its unit FIRST — benchstat emits one table per metric
  # and the unit lives in the header row's second field. This must run before
  # the empty-name skip below, since the header row also has an empty name.
  if [[ -z "$name" && "$vsbase" == "vs base" ]]; then
    current_unit="$_base_val"
    continue
  fi

  # Skip blank lines, goos/goarch/pkg header lines, and path rows.
  [[ -z "$name" ]] && continue
  [[ "$name" == goos* || "$name" == goarch* || "$name" == pkg* ]] && continue

  # Only the sec/op table fails the gate. B/op and allocs/op are logged but do
  # not fail here (the bench-history guard band covers allocations); B/s is the
  # SetBytes throughput table, where a positive delta is an IMPROVEMENT, not a
  # regression — failing on it was a latent bug surfaced by SetBytes benchmarks.
  [[ "$current_unit" != "sec/op" ]] && continue

  # Skip the geomean aggregate row — individual rows already catch it.
  [[ "$name" == geomean* ]] && continue

  # Skip "~" (not significant) and empty delta.
  [[ -z "$vsbase" || "$vsbase" == "~" ]] && continue

  # We only fail on positive (slow-down) deltas.
  # Strip leading '+' and trailing '%'; negative deltas start with '-'.
  pct="${vsbase#+}"   # remove leading '+' if present
  pct="${pct%\%}"     # remove trailing '%'

  # Skip negative deltas (improvements).
  if [[ "$pct" == -* ]]; then
    continue
  fi

  # awk handles floating-point comparison portably.
  over=$(awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { print (p+0 > t+0) ? "1" : "0" }')
  if [[ "$over" == "1" ]]; then
    echo "REGRESSION: ${name} regressed by ${vsbase} (threshold +${THRESHOLD}%)"
    REGRESSIONS=$((REGRESSIONS + 1))
  fi
done <<< "$CSV_OUT"

if (( MISSING > 0 )); then
  echo "::error::${MISSING} baseline headline benchmark(s) disappeared from the head run — the regression gate cannot cover them; restore the benchmark or update run_headline_bench.sh"
  exit 1
fi

if (( REGRESSIONS > 0 )); then
  echo "::error::${REGRESSIONS} headline benchmark(s) regressed beyond +${THRESHOLD}% — see diff above"
  exit 1
fi

echo "All headline benchmarks within +${THRESHOLD}% threshold."
exit 0
