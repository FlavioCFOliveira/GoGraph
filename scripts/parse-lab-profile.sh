#!/usr/bin/env bash
# parse-lab-profile.sh — round-1 PROFILING of the Cypher statement front end
# (rmp #2844, sprint 362). The profiling counterpart of scripts/parse-lab.sh:
# that script measures the stage ladder, this one says where inside each stage
# the CPU and the allocations actually go, and how much of it is the ANTLR
# runtime rather than GoGraph code.
#
# One command reproduces the whole round from a clean tree:
#
#     scripts/parse-lab-profile.sh
#
# Optional arguments:
#     scripts/parse-lab-profile.sh [OUT_DIR] [CPU_BENCHTIME] [MEM_BENCHTIME] [COUNT]
#
# Everything the round needs to be believed is written to OUT_DIR: the loadavg
# before and after EVERY measurement, the environment, the raw benchmark logs
# with go test's exit code read from inside them, the pprof profiles, the folded
# stacks, the flame graphs, the per-package split and the benchstat summaries.
#
# It writes only inside OUT_DIR. It runs no git write operation and changes no
# production code.
#
# NOTE ON OBSERVER EFFECT. The -memprofilerate=1 series records every single
# allocation with a full stack walk, so ITS ns/op IS NOT A LATENCY NUMBER and is
# never quoted as one. It exists to make allocs/op EXACT. Latency comes from the
# unprofiled benchstat series; allocation counts come from here.

set -u -o pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1

OUT_DIR="${1:-docs/benchmarks/cypher-parse-lab-$(date +%Y-%m-%d)-round1}"
CPU_BT="${2:-1s}"
MEM_BT="${3:-200ms}"
COUNT="${4:-10}"

mkdir -p "$OUT_DIR" || exit 1
ABS_OUT="$(cd "$OUT_DIR" && pwd)"
ENV_FILE="$OUT_DIR/env.txt"
FLAME="$REPO/scripts/parse_lab_flamegraph.py"

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
  echo "graphviz_dot=$(command -v dot || echo 'ABSENT — flame graphs are rendered by scripts/parse_lab_flamegraph.py')"
  echo "benchstat=$(command -v benchstat || echo ABSENT)"
  echo "cpu_benchtime=$CPU_BT"
  echo "mem_benchtime=$MEM_BT"
  echo "count=$COUNT"
  echo "corpus=cypher/parser/testdata/examples-corpus.json"
  echo "build_flags=none (no -race, no -tags)"
  echo "antlr_module=$(grep antlr go.mod | tr -s ' ')"
} > "$ENV_FILE"

note() { echo "--- $*"; echo "$*" >> "$OUT_DIR/steps.txt"; }

# cpuprof NAME PKG BENCH — one CPU-profiled series, bracketed by loadavg.
cpuprof() {
  local name="$1" pkg="$2" re="$3"
  uptime > "$OUT_DIR/$name.loadavg-before.txt"
  go test -run='^$' -bench="$re" -benchmem -benchtime="$CPU_BT" -count=1 \
      -cpuprofile="$ABS_OUT/$name.cpu.pb.gz" "$pkg" > "$OUT_DIR/$name.log" 2>&1
  echo "go_test_exit=$?" >> "$OUT_DIR/$name.log"
  uptime > "$OUT_DIR/$name.loadavg-after.txt"
  note "$name cpu: exit=$(sed -n 's/^go_test_exit=//p' "$OUT_DIR/$name.log") lines=$(grep -c '^Benchmark' "$OUT_DIR/$name.log")"
}

# memprof NAME PKG BENCH — one series with EVERY allocation recorded.
memprof() {
  local name="$1" pkg="$2" re="$3"
  uptime > "$OUT_DIR/$name.mem.loadavg-before.txt"
  go test -run='^$' -bench="$re" -benchmem -benchtime="$MEM_BT" -count=1 \
      -memprofilerate=1 -memprofile="$ABS_OUT/$name.mem.pb.gz" \
      "$pkg" > "$OUT_DIR/$name.mem.log" 2>&1
  echo "go_test_exit=$?" >> "$OUT_DIR/$name.mem.log"
  uptime > "$OUT_DIR/$name.mem.loadavg-after.txt"
  note "$name mem: exit=$(sed -n 's/^go_test_exit=//p' "$OUT_DIR/$name.mem.log")"
}

# analyse NAME FOCUS — every derived view of one CPU profile.
analyse_cpu() {
  local name="$1" focus="$2"
  local p="$OUT_DIR/$name.cpu.pb.gz"
  [ -s "$p" ] || { note "$name: NO CPU PROFILE — analysis skipped"; return; }
  go tool pprof -top -nodecount=100000 "$p" > "$OUT_DIR/$name.cpu.top.txt" 2>&1
  go tool pprof -top -nodecount=100000 -focus="$focus" "$p" \
      > "$OUT_DIR/$name.cpu.top.focused.txt" 2>&1
  go tool pprof -top -cum -nodecount=200 -focus="$focus" "$p" \
      > "$OUT_DIR/$name.cpu.topcum.focused.txt" 2>&1
  python3 "$FLAME" "$p" "$ABS_OUT/$name.cpu.flame" > /dev/null 2>&1
  python3 "$FLAME" "$p" "$ABS_OUT/$name.cpu.flame.focused" --focus="$focus" > /dev/null 2>&1
  # CUMULATIVE share of samples whose stack CONTAINS the subject anywhere.
  #
  # `-show` is deliberately NOT used: pprof redistributes a dropped node's cost
  # into its nearest retained ancestor, so `-show=pkg` reports a cumulative-like
  # figure, not that package's flat sum. Verified on this host — the four
  # package shares summed to 0.36s inside a 0.12s focused total. The flat
  # per-package split therefore comes from the full -top table, bucketed by
  # scripts/parse_lab_flamegraph.py, which agrees with the folded -traces route
  # to the last sample.
  : > "$OUT_DIR/$name.cpu.subjects.txt"
  {
    echo "Each line is 'Showing nodes accounting for A, B% of C total' where"
    echo "A is the CUMULATIVE value of every sample whose stack contains the subject"
    echo "and C is the whole profile. The baseline below is the same figure for the"
    echo "benchmark's own stack, which is what A should be read against."
    echo ""
    echo "### baseline focus=$focus"
    go tool pprof -top -nodecount=100000 -focus="$focus" "$p" 2>&1 \
        | grep -E 'Duration|Showing nodes accounting'
  } >> "$OUT_DIR/$name.cpu.subjects.txt"
  for subj in 'github\.com/antlr4-go/antlr' 'cypher/parser/gen\.' \
              'GoGraph/cypher/parser\.' 'runtime\.mallocgc' \
              'runtime\.gcDrain|runtime\.gcBgMarkWorker|runtime\.scanobject' \
              'runtime\.growslice' 'runtime\.mapassign|runtime\.mapaccess' \
              'strings\.ToUpper' 'parser\.StripLiterals' \
              'runtime\.mProf_Malloc|runtime\.profilealloc'; do
    {
      echo ""
      echo "### subject=$subj"
      go tool pprof -top -nodecount=100000 -focus="$subj" "$p" 2>&1 \
          | grep -E 'Showing nodes accounting'
    } >> "$OUT_DIR/$name.cpu.subjects.txt"
  done
}

# analyse_mem NAME FOCUS LISTRE — alloc_objects and alloc_space views.
analyse_mem() {
  local name="$1" focus="$2" listre="$3"
  local p="$OUT_DIR/$name.mem.pb.gz"
  [ -s "$p" ] || { note "$name: NO MEM PROFILE — analysis skipped"; return; }
  for idx in alloc_objects alloc_space inuse_objects inuse_space; do
    go tool pprof -sample_index="$idx" -top -nodecount=100000 "$p" \
        > "$OUT_DIR/$name.mem.$idx.top.txt" 2>&1
    go tool pprof -sample_index="$idx" -top -cum -nodecount=200 -focus="$focus" "$p" \
        > "$OUT_DIR/$name.mem.$idx.topcum.focused.txt" 2>&1
  done
  python3 "$FLAME" "$p" "$ABS_OUT/$name.mem.allocobj.flame" \
      --sample=alloc_objects --focus="$focus" > /dev/null 2>&1
  python3 "$FLAME" "$p" "$ABS_OUT/$name.mem.allocspace.flame" \
      --sample=alloc_space --focus="$focus" > /dev/null 2>&1
  if [ -n "$listre" ]; then
    go tool pprof -sample_index=alloc_objects -list="$listre" "$p" \
        > "$OUT_DIR/$name.mem.list.alloc_objects.txt" 2>&1
    go tool pprof -sample_index=alloc_space -list="$listre" "$p" \
        > "$OUT_DIR/$name.mem.list.alloc_space.txt" 2>&1
  fi
}

# ── 1. CPU profiles ──────────────────────────────────────────────────────────
cpuprof frontend-full ./cypher/parser/ '^BenchmarkFrontEndStage$/^Full$'
cpuprof strip-corpus  ./cypher/parser/ '^BenchmarkStripCorpus$'
cpuprof prepare-cold  ./cypher/        '^BenchmarkPrepareCold$'
cpuprof prepare-warm  ./cypher/        '^BenchmarkPrepareWarm$'

# ── 2. Allocation profiles, every allocation recorded ────────────────────────
memprof frontend-full ./cypher/parser/ '^BenchmarkFrontEndStage$/^Full$'
memprof strip-corpus  ./cypher/parser/ '^BenchmarkStripCorpus$'
memprof prepare-cold  ./cypher/        '^BenchmarkPrepareCold$'
memprof prepare-warm  ./cypher/        '^BenchmarkPrepareWarm$'

# ── 3. Derived views ─────────────────────────────────────────────────────────
analyse_cpu frontend-full 'BenchmarkFrontEndStage'
analyse_cpu strip-corpus  'BenchmarkStripCorpus'
analyse_cpu prepare-cold  'BenchmarkPrepareCold'
analyse_cpu prepare-warm  'BenchmarkPrepareWarm'

analyse_mem frontend-full 'BenchmarkFrontEndStage' 'ParseStatement'
analyse_mem strip-corpus  'BenchmarkStripCorpus'   'StripLiterals'
analyse_mem prepare-cold  'BenchmarkPrepareCold'   'compilePlanCacheEntry'
analyse_mem prepare-warm  'BenchmarkPrepareWarm'   'StripLiterals|parseAndAnalyse'

# ── 4. GC behaviour of the cold front end ────────────────────────────────────
uptime > "$OUT_DIR/gctrace.loadavg-before.txt"
GODEBUG=gctrace=1 go test -run='^$' -bench='^BenchmarkPrepareCold$' -benchmem \
    -benchtime=200ms -count=1 ./cypher/ > "$OUT_DIR/gctrace-prepare-cold.txt" 2>&1
echo "go_test_exit=$?" >> "$OUT_DIR/gctrace-prepare-cold.txt"
uptime > "$OUT_DIR/gctrace.loadavg-after.txt"
note "gctrace: exit=$(sed -n 's/^go_test_exit=//p' "$OUT_DIR/gctrace-prepare-cold.txt") gc_lines=$(grep -c '^gc ' "$OUT_DIR/gctrace-prepare-cold.txt")"

# ── 5. The -count=COUNT benchstat series ─────────────────────────────────────
# series NAME PKG BENCH
series() {
  local name="$1" pkg="$2" re="$3"
  uptime > "$OUT_DIR/$name.series.loadavg-before.txt"
  go test -run='^$' -bench="$re" -benchmem -benchtime=100ms -count="$COUNT" \
      "$pkg" > "$OUT_DIR/$name.series.txt" 2>&1
  echo "go_test_exit=$?" >> "$OUT_DIR/$name.series.txt"
  uptime > "$OUT_DIR/$name.series.loadavg-after.txt"
  if command -v benchstat >/dev/null 2>&1; then
    benchstat "$OUT_DIR/$name.series.txt" > "$OUT_DIR/$name.benchstat.txt" 2>&1
    echo "benchstat_exit=$?" >> "$OUT_DIR/$name.benchstat.txt"
  fi
  note "$name series: exit=$(sed -n 's/^go_test_exit=//p' "$OUT_DIR/$name.series.txt") lines=$(grep -c '^Benchmark' "$OUT_DIR/$name.series.txt")"
}

series frontend-share ./cypher/        '^BenchmarkFrontEndShare$'
series strip-shape    ./cypher/parser/ '^BenchmarkStripShape$'

# ── 6. The stage ladder at -count=COUNT, through stage 1's own driver ────────
note "stage ladder at count=$COUNT via scripts/parse-lab.sh"
scripts/parse-lab.sh "$OUT_DIR/lab" "$COUNT" 100ms > "$OUT_DIR/lab-driver.log" 2>&1
echo "parse_lab_exit=$?" >> "$OUT_DIR/lab-driver.log"
note "lab: $(sed -n 's/^parse_lab_exit=//p' "$OUT_DIR/lab-driver.log")"

# ── 7. A real end-to-end program under pprof ─────────────────────────────────
uptime > "$OUT_DIR/example22.loadavg-before.txt"
mkdir -p "$ABS_OUT/example22"
go run ./examples/22_cypher -users 50000 -knows-max 8 -seed 7 \
    -profile-dir "$ABS_OUT/example22" -trace "$ABS_OUT/example22/trace.out" \
    > "$OUT_DIR/example22.log" 2>&1
echo "go_run_exit=$?" >> "$OUT_DIR/example22.log"
uptime > "$OUT_DIR/example22.loadavg-after.txt"
note "example22: exit=$(sed -n 's/^go_run_exit=//p' "$OUT_DIR/example22.log")"
if [ -s "$OUT_DIR/example22/cpu.pprof" ]; then
  go tool pprof -top -nodecount=100000 "$OUT_DIR/example22/cpu.pprof" \
      > "$OUT_DIR/example22.cpu.top.txt" 2>&1
  go tool pprof -top -cum -nodecount=200 -focus='Engine.*runRead|Engine.*Run$' \
      "$OUT_DIR/example22/cpu.pprof" > "$OUT_DIR/example22.cpu.runread.txt" 2>&1
  python3 "$FLAME" "$OUT_DIR/example22/cpu.pprof" "$ABS_OUT/example22.cpu.flame" \
      > /dev/null 2>&1
fi

echo "artefacts in $OUT_DIR"
