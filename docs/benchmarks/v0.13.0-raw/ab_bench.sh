#!/usr/bin/env bash
# Interleaved A/B benchmark harness: v0.12.0 (base) vs release/0.13.0 (head).
#
# WHY interleaved rather than all-A-then-all-B: a host's thermal and scheduling
# state drifts over a long run. Running one arm to completion and then the other
# assigns that drift entirely to the second arm. Alternating one repetition at a
# time distributes it across both, so what benchstat sees as a delta is the code.
#
# WHY pre-compiled binaries: `go test -bench` would recompile per invocation, and
# a rebuild between repetitions puts compiler CPU inside the measurement window.
# Each arm is compiled ONCE, from its own tree, and the same binary runs every
# repetition.
#
# WHY the load gate: on this host a 1-minute load average above ~2.5 is a
# systematic bias, not noise (established 2026-08-10, docs/benchmarks/v0.12.0.md).
# Every round records the figure it started at, so the report can be audited.
set -u -o pipefail

SP="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Overridable so the noise-floor block can point BOTH arms at the same tree:
# two binaries built from identical source must differ only by noise, which
# is exactly the floor every later delta has to clear.
BASE_TREE="${BASE_TREE:-$SP/base-v0.12.0}"
HEAD_TREE="${HEAD_TREE:-/Users/flaviocfo/dev/xumiga/GoGraph}"
ARM_SUFFIX="${ARM_SUFFIX:-}"
OUT="$SP/ab"
BIN="$SP/binaries"
LOAD_MAX="${LOAD_MAX:-2.5}"
LOAD_WAIT_MAX="${LOAD_WAIT_MAX:-900}"

mkdir -p "$OUT" "$BIN"

loadavg1() { sysctl -n vm.loadavg | tr ',' '.' | awk '{print $2}'; }

wait_for_quiet() {  # $1 = label for the log
  local waited=0 la
  la=$(loadavg1)
  while awk -v a="$la" -v m="$LOAD_MAX" 'BEGIN{exit !(a>m)}'; do
    if [ "$waited" -ge "$LOAD_WAIT_MAX" ]; then
      echo "LOAD-GATE-TIMEOUT $1 loadavg=$la after ${waited}s" >&2
      return 1
    fi
    sleep 30; waited=$((waited+30)); la=$(loadavg1)
  done
  echo "LOAD-GATE $1 started_at=$la waited=${waited}s"
  return 0
}

# compile_arm <arm> <tree> <pkg>  -> prints binary path, or fails
compile_arm() {
  local arm="$1" tree="$2" pkg="$3"
  local safe="${pkg//\//_}"; safe="${safe#._}"
  local out="$BIN/${arm}${ARM_SUFFIX}__${safe}.test"
  if [ ! -x "$out" ]; then
    ( cd "$tree" && go test -c -o "$out" "$pkg" ) >"$BIN/${arm}${ARM_SUFFIX}__${safe}.build.log" 2>&1 \
      || { echo "BUILD-FAILED arm=$arm pkg=$pkg (see $BIN/${arm}${ARM_SUFFIX}__${safe}.build.log)" >&2; return 1; }
  fi
  echo "$out"
}

# run_ab <label> <pkg> <bench-regex> <count> [extra test flags...]
run_ab() {
  local label="$1" pkg="$2" regex="$3" count="$4"; shift 4
  local extra=("$@")
  local base_bin head_bin
  base_bin=$(compile_arm base "$BASE_TREE" "$pkg") || return 1
  head_bin=$(compile_arm head "$HEAD_TREE" "$pkg") || return 1

  local bf="$OUT/${label}.base.txt" hf="$OUT/${label}.head.txt" lg="$OUT/${label}.load.log"
  local ef="$OUT/${label}.stderr.log"; : > "$ef"
  local RESULT_RE='^Benchmark[^[:space:]]*[[:space:]]+[0-9]+[[:space:]]+[0-9.]+[[:space:]]+[A-Za-z/]+'
  : > "$bf"; : > "$hf"; : > "$lg"
  # benchstat reads the goos/goarch/pkg header; write it once per file.
  { echo "goos: darwin"; echo "goarch: arm64"; echo "pkg: $pkg"; } | tee -a "$bf" >> "$hf"

  # Gate ONCE, before the block opens. Gating per round would DEADLOCK: a
  # parallel benchmark legitimately drives the 1-minute load average to ~10 on
  # this 10-core host, so round 2 would wait forever on load the harness itself
  # produced. What the gate protects against is starting a block on a machine
  # that is busy with something ELSE; once the block is running, its own load is
  # the point. Each round still RECORDS its load average, so the report can show
  # the block's trajectory and a reader can audit it.
  wait_for_quiet "$label block-open" >> "$lg" || return 1

  local i
  for i in $(seq 1 "$count"); do
    echo "round=$i load1_at_round_start=$(loadavg1)" >> "$lg"
    # A fixed settle before each round, applied identically to both arms, so
    # neither inherits more of the previous round's decay than the other.
    sleep 3
    # BASE first on odd rounds, HEAD first on even rounds, so neither arm
    # systematically inherits the other's cache and thermal aftermath.
    if [ $((i % 2)) -eq 1 ]; then
      "$base_bin" -test.run='^$' -test.bench="$regex" -test.benchmem -test.count=1 ${extra[@]+"${extra[@]}"} 2>>"$ef" | grep -E "$RESULT_RE" >> "$bf"
      "$head_bin" -test.run='^$' -test.bench="$regex" -test.benchmem -test.count=1 ${extra[@]+"${extra[@]}"} 2>>"$ef" | grep -E "$RESULT_RE" >> "$hf"
    else
      "$head_bin" -test.run='^$' -test.bench="$regex" -test.benchmem -test.count=1 ${extra[@]+"${extra[@]}"} 2>>"$ef" | grep -E "$RESULT_RE" >> "$hf"
      "$base_bin" -test.run='^$' -test.bench="$regex" -test.benchmem -test.count=1 ${extra[@]+"${extra[@]}"} 2>>"$ef" | grep -E "$RESULT_RE" >> "$bf"
    fi
    echo "round=$i base_lines=$(grep -c '^Benchmark' "$bf") head_lines=$(grep -c '^Benchmark' "$hf")" >> "$lg"
  done

  # A result line count of zero means the regex matched nothing in that arm —
  # a silent no-op that would otherwise be reported as "no change".
  local bn hn
  bn=$(grep -c '^Benchmark' "$bf"); hn=$(grep -c '^Benchmark' "$hf")
  if [ "$bn" -eq 0 ] || [ "$hn" -eq 0 ]; then
    echo "EMPTY-RESULT label=$label base=$bn head=$hn — regex matched nothing in one arm" >&2
    return 1
  fi
  echo "OK label=$label base_lines=$bn head_lines=$hn"
}

"$@"
