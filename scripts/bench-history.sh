#!/usr/bin/env bash
# bench-history.sh — empirical performance-history harness.
#
# Records a curated, fixed set of hot-path benchmarks into a versioned
# history directory and compares every run against the previous one with
# benchstat, so the gain or regression introduced by each change is captured
# empirically and reviewable over time.
#
# The curated set is deliberately small and stable: it covers the Cypher
# result-materialisation read path (the current optimisation target), the
# Cypher write path, the per-operator allocation gates, and a guard band of
# graph-algorithm benchmarks so an optimisation in one layer cannot silently
# regress another. Adding entries lengthens every run — only hot-path-critical
# benchmarks belong here.
#
# Usage:
#   ./scripts/bench-history.sh <label> [count] [benchtime]
#
#   <label>     short kebab-case tag for this run, e.g. "baseline" or
#               "opt1-nodeid-accessors". Required.
#   [count]     -count passed to go test (default 6 — enough samples for
#               benchstat confidence intervals).
#   [benchtime] -benchtime passed to go test (default 1s).
#
# Output:
#   docs/benchmarks/history/NNNN__<label>__<shortcommit>.txt   raw benchmark output
#   docs/benchmarks/history/NNNN__<label>__<shortcommit>.delta.txt   benchstat vs previous run
#   docs/benchmarks/history/LEDGER.md   one appended row per run
#
# The script never fails the build on a regression; it surfaces the delta so a
# human (or the agent driving the optimisation) decides whether to accept it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
HIST_DIR="${REPO_ROOT}/docs/benchmarks/history"
LEDGER="${HIST_DIR}/LEDGER.md"

LABEL="${1:-}"
COUNT="${2:-6}"
BENCHTIME="${3:-1s}"

if [[ -z "$LABEL" ]]; then
  echo "usage: $0 <label> [count] [benchtime]" >&2
  exit 2
fi

BENCHSTAT="$(command -v benchstat || echo "$(go env GOPATH)/bin/benchstat")"
if [[ ! -x "$BENCHSTAT" ]]; then
  echo "benchstat not found; install with: go install golang.org/x/perf/cmd/benchstat@latest" >&2
  exit 2
fi

mkdir -p "$HIST_DIR"

# ── Curated benchmark set ────────────────────────────────────────────────────
# Each entry is "PACKAGE :: REGEX". Packages are listed once even when several
# regexes target them so go test compiles each package a single time.
declare -a SUITE=(
  "./bench/cypher_ldbc/... :: ^(BenchmarkIC1|BenchmarkIC2|BenchmarkIC3|BenchmarkIC4|BenchmarkIC5|BenchmarkIC6|BenchmarkIC7|BenchmarkIC8|BenchmarkIC9|BenchmarkIC10|BenchmarkIC11|BenchmarkIC12|BenchmarkIC13|BenchmarkIC14|BenchmarkWithProjection)$"
  "./bench/cypher_alloc/... :: ^(BenchmarkProjectOp|BenchmarkResultSet|BenchmarkAllNodesScan)$"
  "./search/... :: ^(BenchmarkDijkstra_PostWarmup|BenchmarkDijkstra_Large|BenchmarkBFSDirectionOpt_PowerLaw)$"
  "./search/centrality/... :: ^(BenchmarkBrandes_RandomGraph|BenchmarkPageRank_PowerLaw50K)$"
)

COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)"
DIRTY=""
if ! git -C "$REPO_ROOT" diff --quiet 2>/dev/null || ! git -C "$REPO_ROOT" diff --cached --quiet 2>/dev/null; then
  DIRTY="-dirty"
fi

# Next zero-padded sequence number.
#
# Derived from the raw files on disk AND from the LEDGER, taking the maximum of
# the two. The LEDGER is consulted because a run's raw .txt is not always
# committed — several are not — and deriving the sequence from files alone then
# silently REUSES a number the ledger has already spent. That happened on
# 2026-07-28: rows 0024 and 0025 existed (sprints 309 and 310) with no raw files,
# so a new run claimed 0025 a second time and appended a duplicate row.
SEQ=1
shopt -s nullglob
for f in "$HIST_DIR"/[0-9][0-9][0-9][0-9]__*.txt; do
  base="$(basename "$f")"
  n="${base%%__*}"
  n=$((10#$n))
  if (( n >= SEQ )); then SEQ=$((n + 1)); fi
done
shopt -u nullglob
if [[ -f "$LEDGER" ]]; then
  while read -r n; do
    [[ -z "$n" ]] && continue
    n=$((10#$n))
    if (( n >= SEQ )); then SEQ=$((n + 1)); fi
  done < <(grep -oE '^\| [0-9]{4}' "$LEDGER" | awk '{print $2}')
fi
SEQ_PAD="$(printf '%04d' "$SEQ")"

# Most recent prior raw run (for benchstat comparison).
PREV=""
shopt -s nullglob
prevs=( "$HIST_DIR"/[0-9][0-9][0-9][0-9]__*.txt )
shopt -u nullglob
# Exclude any .delta.txt that matched the glob.
filtered=()
for f in "${prevs[@]+"${prevs[@]}"}"; do
  [[ "$f" == *.delta.txt ]] && continue
  filtered+=( "$f" )
done
if (( ${#filtered[@]} > 0 )); then
  PREV="${filtered[$(( ${#filtered[@]} - 1 ))]}"
fi

OUT="${HIST_DIR}/${SEQ_PAD}__${LABEL}__${COMMIT}${DIRTY}.txt"

# strip_log_noise repairs `go test -bench` output that a library log line has
# interleaved with.
#
# `go test` forwards the test binary's STDERR into its own STDOUT, so anything the
# code under benchmark logs lands in the middle of the benchmark stream. The
# damaging case is not the extra lines — benchstat ignores what it cannot parse —
# but that a log line arriving between go test's two writes SPLITS a benchmark's
# name from its result:
#
#   BenchmarkIC10-10   2026/07/28 13:17:52 WARN cypher: engine constructed over …
#   2026/07/28 13:17:52 WARN cypher: …
#      69986     17682 ns/op   57600 B/op   84 allocs/op
#
# benchstat needs `name  iters  value unit …` on ONE line, so every split sample
# is silently dropped — and a benchstat that silently sees fewer benchmarks reads
# exactly like "no regression". A curated run measured on 2026-07-28 had 90 of its
# 92 result lines split this way, which would have made the history file worthless
# while looking clean (runs 0001–0022 predate the log line and are unaffected).
#
# This filter deletes any `YYYY/MM/DD HH:MM:SS …` text to end of line, drops lines
# that become blank, and re-joins a bare benchmark-name line with the result line
# that follows. It touches only how output is RECORDED — never a measurement — and
# it is deliberately generic, so any future logging on a benchmarked path is
# handled without another repair.
strip_log_noise() {
  awk '
    {
      line = $0
      # Any timestamped log record, wherever it starts on the line.
      sub(/[0-9]+\/[0-9]+\/[0-9]+ [0-9]+:[0-9]+:[0-9]+ .*$/, "", line)
      sub(/[ \t]+$/, "", line)
      if (line == "") next
      if (line ~ /^Benchmark[^ \t]*$/) { pending = line; next }
      if (pending != "") { print pending "\t" line; pending = ""; next }
      print line
    }
    END { if (pending != "") print pending }
  '
}

echo "── bench-history run ${SEQ_PAD} (${LABEL}) @ ${COMMIT}${DIRTY} ──"
echo "count=${COUNT} benchtime=${BENCHTIME}"
echo

: > "$OUT"
for entry in "${SUITE[@]}"; do
  pkg="${entry%% :: *}"
  regex="${entry##* :: }"
  echo ">>> ${pkg}  ${regex}"
  ( cd "$REPO_ROOT" && go test -run='^$' -bench="$regex" -benchmem \
      -count="$COUNT" -benchtime="$BENCHTIME" "$pkg" ) | strip_log_noise | tee -a "$OUT"
  echo
done

echo "raw results → $OUT"

if [[ -n "$PREV" && "$PREV" != "$OUT" ]]; then
  DELTA="${HIST_DIR}/${SEQ_PAD}__${LABEL}__${COMMIT}${DIRTY}.delta.txt"
  echo
  echo "── benchstat: $(basename "$PREV") → $(basename "$OUT") ──"
  if "$BENCHSTAT" "$PREV" "$OUT" > "$DELTA" 2>/dev/null; then
    cat "$DELTA"
    echo "delta → $DELTA"
  else
    echo "(benchstat could not compare — benchmark set names may have changed)"
  fi
fi

# ── Ledger ───────────────────────────────────────────────────────────────────
if [[ ! -f "$LEDGER" ]]; then
  cat > "$LEDGER" <<'EOF'
# Performance history ledger

Each row is one `bench-history.sh` run. The raw numbers live in the
`NNNN__<label>__<commit>.txt` file; the benchstat comparison against the
previous run lives in the matching `.delta.txt`. Fill the **Summary** column
with the one-line outcome of the change (the headline delta from the
`.delta.txt`), so the table reads as a chronological record of gains and
regressions.

| Seq | Date (UTC) | Commit | Label | Summary |
|----:|-----------|--------|-------|---------|
EOF
fi
printf '| %s | %s | `%s` | %s | _pending — see %s.delta.txt_ |\n' \
  "$SEQ_PAD" "$(date -u +%Y-%m-%d)" "${COMMIT}${DIRTY}" "$LABEL" \
  "${SEQ_PAD}__${LABEL}__${COMMIT}${DIRTY}" >> "$LEDGER"

echo
echo "ledger row appended → $LEDGER"
