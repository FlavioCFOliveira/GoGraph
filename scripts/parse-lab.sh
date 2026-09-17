#!/usr/bin/env bash
# parse-lab.sh — the Cypher statement-parsing laboratory (rmp #2843, sprint 362).
#
# Measures the Cypher front end stage by stage over the statement corpus
# harvested from the project's own examples
# (cypher/parser/testdata/examples-corpus.json), on both the cold path (a
# statement the plan cache has never seen) and the warm path (a statement the
# plan cache holds).
#
# One command reproduces the whole run from a clean tree:
#
#     scripts/parse-lab.sh
#
# Optional arguments:
#     scripts/parse-lab.sh [OUT_DIR] [COUNT] [BENCHTIME]
#
# Everything the run needs to be believed is written to OUT_DIR: the loadavg
# before and after each measurement, the Go version, GOMAXPROCS, the commit, the
# raw benchmark output (which is benchstat's input format), the benchstat
# summary, and a per-stage table.
#
# It writes only inside OUT_DIR. It runs no git write operation.

set -u -o pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1

OUT_DIR="${1:-docs/benchmarks/cypher-parse-lab-$(date +%Y-%m-%d)-raw}"
COUNT="${2:-8}"
BENCHTIME="${3:-100ms}"

mkdir -p "$OUT_DIR" || exit 1
ENV_FILE="$OUT_DIR/env.txt"

{
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit=$(git rev-parse HEAD)"
  echo "branch=$(git rev-parse --abbrev-ref HEAD)"
  echo "tree_dirty_files=$(git status --porcelain | wc -l | tr -d ' ')"
  git status --porcelain | sed 's/^/tree_dirty: /' 
  echo "go_version=$(go version)"
  echo "gomaxprocs_env=${GOMAXPROCS:-unset}"
  echo "num_cpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)"
  echo "os=$(uname -srm)"
  echo "hw=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
  echo "mem_bytes=$(sysctl -n hw.memsize 2>/dev/null || echo unknown)"
  echo "count=$COUNT"
  echo "benchtime=$BENCHTIME"
  echo "corpus=cypher/parser/testdata/examples-corpus.json"
  echo "corpus_statements=$(python3 -c 'import json,sys;print(len(json.load(open("cypher/parser/testdata/examples-corpus.json"))["statements"]))')"
  echo "build_flags=none (no -race, no -tags)"
} > "$ENV_FILE"

# run NAME PKG BENCH_REGEX — one measured series, bracketed by loadavg.
run() {
  local name="$1" pkg="$2" re="$3"
  local log="$OUT_DIR/$name.txt"
  uptime > "$OUT_DIR/$name.loadavg-before.txt"
  go test -run='^$' -bench="$re" -benchmem \
      -benchtime="$BENCHTIME" -count="$COUNT" "$pkg" > "$log" 2>&1
  echo "go_test_exit=$?" >> "$log"
  uptime > "$OUT_DIR/$name.loadavg-after.txt"
  echo "--- $name: $(grep -c '^Benchmark' "$log") benchmark lines, $(tail -1 "$log")"
  echo "${name}_gomaxprocs_effective=$(sed -n 's/^Benchmark[^ ]*-\([0-9][0-9]*\)[[:space:]].*/\1/p' "$log" | head -1)" >> "$ENV_FILE"
  echo "${name}_benchmark_lines=$(grep -c '^Benchmark' "$log")" >> "$ENV_FILE"
  echo "${name}_go_test_exit=$(sed -n 's/^go_test_exit=//p' "$log")" >> "$ENV_FILE"
}

run stage   ./cypher/parser/ '^BenchmarkFrontEndStage$'
run prepare ./cypher/        '^(BenchmarkPrepareCold|BenchmarkPrepareWarm|BenchmarkStageSema)$'

# benchstat summary — the canonical, comparable form of the same two logs.
if command -v benchstat >/dev/null 2>&1; then
  benchstat "$OUT_DIR/stage.txt"   > "$OUT_DIR/stage.benchstat.txt"   2>&1
  echo "benchstat_exit=$?" >> "$OUT_DIR/stage.benchstat.txt"
  benchstat "$OUT_DIR/prepare.txt" > "$OUT_DIR/prepare.benchstat.txt" 2>&1
  echo "benchstat_exit=$?" >> "$OUT_DIR/prepare.benchstat.txt"
else
  echo "benchstat not installed; raw logs are its input format" > "$OUT_DIR/stage.benchstat.txt"
fi

# Per-stage table: medians across the -count series, plus the two stage costs
# that exist only as a difference between cumulative prefixes.
python3 scripts/parse_lab_table.py "$OUT_DIR" || exit 1

echo "artefacts in $OUT_DIR"
