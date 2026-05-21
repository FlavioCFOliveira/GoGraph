#!/usr/bin/env bash
# profile-cypher.sh — capture CPU and heap profiles for IC1, IC5, IC9 benchmarks.
#
# Usage: ./scripts/profile-cypher.sh [output-dir]
#
# Profiles are written to output-dir (default: /tmp/gograph-profiles).
# For each benchmark a CPU profile, heap profile, and raw benchmark output
# are captured; the top-5 functions from each profile are printed inline.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
OUTDIR="${1:-/tmp/gograph-profiles}"
mkdir -p "$OUTDIR"

for bench in BenchmarkIC1 BenchmarkIC5 BenchmarkIC9; do
  lower=$(echo "$bench" | tr '[:upper:]' '[:lower:]' | sed 's/benchmark//')
  echo "Profiling $bench..."
  (
    cd "$REPO_ROOT"
    go test -bench="^${bench}$" -benchmem -count=1 \
      -cpuprofile="${OUTDIR}/${lower}_cpu.prof" \
      -memprofile="${OUTDIR}/${lower}_mem.prof" \
      ./bench/cypher_ldbc/... > "${OUTDIR}/${lower}_bench.txt" 2>&1
  )
  echo "  CPU top-5:"
  go tool pprof -top -nodecount=5 "${OUTDIR}/${lower}_cpu.prof" 2>/dev/null | tail -7
  echo "  Heap top-5:"
  go tool pprof -top -nodecount=5 "${OUTDIR}/${lower}_mem.prof" 2>/dev/null | tail -7
done

echo "Profiles written to $OUTDIR"
