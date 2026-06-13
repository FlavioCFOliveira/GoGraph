#!/usr/bin/env bash
# test_bench_gate.sh — self-contained gate test for bench_gate.sh.
#
# Verifies two properties:
#   1. A synthetic +100% regression causes bench_gate.sh to exit non-zero (FAIL).
#   2. Clean benchstat output (no regression) causes bench_gate.sh to exit 0 (PASS).
#
# Run directly:
#   bash scripts/test_bench_gate.sh
#
# The test also validates that the gate requires benchstat to be available.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="${SCRIPT_DIR}/bench_gate.sh"

if [[ ! -x "$GATE" ]]; then
  echo "FAIL: bench_gate.sh not found or not executable at ${GATE}" >&2
  exit 1
fi

BENCHSTAT="$(command -v benchstat 2>/dev/null || echo "$(go env GOPATH)/bin/benchstat" 2>/dev/null || echo "")"
if [[ -z "$BENCHSTAT" || ! -x "$BENCHSTAT" ]]; then
  echo "SKIP: benchstat not installed; install with: go install golang.org/x/perf/cmd/benchstat@latest" >&2
  exit 0
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# ── helpers ──────────────────────────────────────────────────────────────────

# Writes 6 samples of BenchmarkDijkstra_PostWarmup at <ns_per_op> ns/op.
write_bench() {
  local file="$1"
  local ns="$2"
  cat > "$file" <<EOF
goos: linux
goarch: amd64
pkg: github.com/FlavioCFOliveira/GoGraph/search
BenchmarkDijkstra_PostWarmup-8   1000   $((ns + 0)) ns/op   512 B/op   8 allocs/op
BenchmarkDijkstra_PostWarmup-8   1000   $((ns + 1000)) ns/op   512 B/op   8 allocs/op
BenchmarkDijkstra_PostWarmup-8   1000   $((ns - 1000)) ns/op   512 B/op   8 allocs/op
BenchmarkDijkstra_PostWarmup-8   1000   $((ns + 500)) ns/op   512 B/op   8 allocs/op
BenchmarkDijkstra_PostWarmup-8   1000   $((ns - 500)) ns/op   512 B/op   8 allocs/op
BenchmarkDijkstra_PostWarmup-8   1000   $((ns + 200)) ns/op   512 B/op   8 allocs/op
EOF
}

PASS_COUNT=0
FAIL_COUNT=0

check() {
  local desc="$1"
  local want="$2"  # "fail" or "pass"
  shift 2
  local rc=0
  "$@" > /dev/null 2>&1 || rc=$?

  if [[ "$want" == "fail" ]]; then
    if (( rc != 0 )); then
      echo "PASS: ${desc} (exit ${rc}, expected non-zero)"
      PASS_COUNT=$((PASS_COUNT + 1))
    else
      echo "FAIL: ${desc} (exit 0, expected non-zero)"
      FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
  else
    if (( rc == 0 )); then
      echo "PASS: ${desc} (exit 0)"
      PASS_COUNT=$((PASS_COUNT + 1))
    else
      echo "FAIL: ${desc} (exit ${rc}, expected 0)"
      FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
  fi
}

# ── Test 1: +100% regression must fail ───────────────────────────────────────
write_bench "$TMPDIR/base.txt"   1000000   # 1 ms/op baseline
write_bench "$TMPDIR/head100.txt" 2000000  # 2 ms/op (100% slower)

check "+100% regression detected as FAIL" fail \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head100.txt" 10

# ── Test 2: +9% is within threshold, must pass ───────────────────────────────
write_bench "$TMPDIR/head9.txt" 1090000  # +9% over baseline

check "+9% within threshold passes" pass \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head9.txt" 10

# ── Test 3: identical runs (no delta) must pass ──────────────────────────────
write_bench "$TMPDIR/head0.txt" 1000000

check "identical runs pass" pass \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head0.txt" 10

# ── Test 4: improvement (-50%) must pass ─────────────────────────────────────
write_bench "$TMPDIR/head_fast.txt" 500000  # 0.5 ms/op (50% faster)

check "50% improvement passes" pass \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head_fast.txt" 10

# ── Test 5: missing benchstat input must exit 2 (usage error) ────────────────
check "missing head file exits non-zero" fail \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/nonexistent.txt" 10

# ── Test 6: benchmark vanished from head (empty head) must FAIL ───────────────
# Reproduces the silent un-gating: with no comparison row benchstat emits zero
# deltas, so a pre-#1447 gate exited 0. The disappearance guard must fail.
: > "$TMPDIR/head_empty.txt"  # head ran no benchmarks at all
check "headline benchmark vanished (empty head) FAILs" fail \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head_empty.txt" 10

# ── Test 7: headline benchmark renamed in head must FAIL ──────────────────────
# A rename leaves the baseline name absent from head even though benchstat
# succeeds on the (differently-named) head data — the hot path is no longer
# gated. The superset check must catch it.
sed 's/BenchmarkDijkstra_PostWarmup/BenchmarkDijkstra_Renamed/g' \
  "$TMPDIR/base.txt" > "$TMPDIR/head_renamed.txt"
check "headline benchmark renamed in head FAILs" fail \
  bash "$GATE" "$TMPDIR/base.txt" "$TMPDIR/head_renamed.txt" 10

# ── Summary ──────────────────────────────────────────────────────────────────
echo
echo "Results: ${PASS_COUNT} passed, ${FAIL_COUNT} failed"

if (( FAIL_COUNT > 0 )); then
  exit 1
fi
exit 0
