#!/usr/bin/env bash
# examples-lab.sh — the whole-surface examples laboratory (rmp #2856, sprint 362).
#
# Drives EVERY program under examples/ one at a time, under pprof, and records
# each run as one row of one manifest: the exact argv, the scale, the host
# loadavg before and after, the elapsed time, the exit code read from inside the
# run's own log, the artefact sizes, and the TOTAL of every profile the run
# produced. A row whose run produced no profile, or a profile whose total is
# zero, is marked FAIL and counted; the script exits non-zero if any row failed.
#
#     scripts/examples-lab.sh OUT_DIR PASS [TIMEOUT_MULTIPLIER]
#
# PASS selects what the sweep is for. The passes are deliberately separate runs
# rather than one run with everything switched on, because each instrument
# perturbs the others:
#
#   default      every example at its DEFAULT scale, plain binaries,
#                -profile-dir -contention 1. Allocation and heap are counted,
#                not sampled, so they are valid at any scale; contention is
#                collected here because the contention samplers must not be
#                charged to a CPU-attribution run.
#   elevated     every example that exposes a scale knob, at an ELEVATED scale,
#                plain binaries, -profile-dir only. This is the CPU-attribution
#                pass: nothing but the CPU profiler is running.
#   contention   the concurrent examples only, at the ELEVATED scale, with
#                -contention 1. Paired with `elevated` it is also the
#                observer-effect control: the same example at the same scale,
#                once with the contention samplers and once without.
#   cover-default / cover-elevated
#                the same two scales driven through binaries built with
#                `go build -cover`, with GOCOVERDIR set, for rmp #2858. A
#                separate pass because the coverage counters perturb both the
#                CPU and the heap profile, so they must never be the binaries a
#                profile is read from.
#
# Binaries are BUILT ONCE, up front, outside every timed window. The previous
# whole-examples sweep timed `go run`, and one example's 4 m 11 s build landed
# inside its measured elapsed time.
#
# Every run is BOUNDED, by a watchdog in THIS script that escalates SIGTERM to
# SIGKILL. The bound is enforced from the parent deliberately, because the
# obvious alternatives do not hold:
#
#   * This host has neither timeout(1) nor gtimeout.
#   * perl's alarm(2) survives execve and looked like a drop-in replacement, and
#     it does bound a shell. It DOES NOT bound a Go program: measured here,
#     examples/01_basic ran 11 min 18 s at 86% CPU against a 300 s alarm,
#     because the Go runtime installs its own SIGALRM disposition. A bound that
#     the subject can decline is not a bound.
#
# SIGKILL cannot be declined. An overrun row is recorded with its reason and the
# elapsed time it actually consumed, never as a missing row.
#
# Two environment variables exist for scripts/test_examples-lab.sh and for
# nothing else:
#
#   LAB_ONLY=REGEX   run only the rows whose id matches REGEX.
#   LAB_BREAK=1      append a flag no example accepts, so the run fails and
#                    writes no profile. This is how the self-test proves a broken
#                    invocation is reported as a FAILED row rather than passing
#                    silently, which is the one property of a driver that cannot
#                    be taken on trust.
#
# LAB_REBUILD=1 forces a rebuild of the binaries the pass uses.
#
# It writes only inside OUT_DIR, which must be outside the repository, and it
# runs no git operation and changes no production code.

set -u -o pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE="github.com/FlavioCFOliveira/GoGraph"

if [ "$#" -lt 2 ]; then
  sed -n '2,40p' "${BASH_SOURCE[0]}" >&2
  exit 2
fi

OUT_DIR="$1"
PASS="$2"
TMUL="${3:-1}"

case "$PASS" in
  default | elevated | contention | cover-default | cover-elevated) ;;
  *)
    echo "unknown pass $PASS" >&2
    exit 2
    ;;
esac

mkdir -p "$OUT_DIR" || exit 1
OUT_DIR="$(cd "$OUT_DIR" && pwd)"
case "$OUT_DIR" in
  "$REPO"/*)
    echo "OUT_DIR $OUT_DIR is inside the repository; artefacts must live outside it" >&2
    exit 2
    ;;
esac

BIN_DIR="$OUT_DIR/../bin"
COVBIN_DIR="$OUT_DIR/../bincov"
mkdir -p "$BIN_DIR" "$COVBIN_DIR" || exit 1
BIN_DIR="$(cd "$BIN_DIR" && pwd)"
COVBIN_DIR="$(cd "$COVBIN_DIR" && pwd)"

PASS_DIR="$OUT_DIR/$PASS"
mkdir -p "$PASS_DIR" || exit 1
MANIFEST="$PASS_DIR/manifest.tsv"
STEPS="$PASS_DIR/steps.txt"
PROBE="$REPO/scripts/examples_lab_probe.py"

# Every example writes its scratch store under TMPDIR via os.MkdirTemp. Point it
# inside OUT_DIR so the sweep's disk footprint is bounded, measurable, and
# removed with the artefact root — and, above all, so nothing lands in the repo.
export TMPDIR="$OUT_DIR/tmp"
mkdir -p "$TMPDIR" || exit 1

COVDIR="$PASS_DIR/covdata"
case "$PASS" in
  cover-*)
    mkdir -p "$COVDIR" || exit 1
    RUN_BIN_DIR="$COVBIN_DIR"
    ;;
  *) RUN_BIN_DIR="$BIN_DIR" ;;
esac

# ─── environment, recorded once, before anything runs ────────────────────────
{
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "pass=$PASS"
  echo "timeout_multiplier=$TMUL"
  echo "commit=$(git -C "$REPO" rev-parse HEAD)"
  echo "branch=$(git -C "$REPO" rev-parse --abbrev-ref HEAD)"
  echo "tree_dirty_files=$(git -C "$REPO" status --porcelain | wc -l | tr -d ' ')"
  git -C "$REPO" status --porcelain | sed 's/^/tree_dirty: /'
  echo "go_version=$(go version)"
  echo "num_cpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)"
  echo "os=$(uname -srm)"
  echo "hw=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
  echo "mem_bytes=$(sysctl -n hw.memsize 2>/dev/null || echo unknown)"
  echo "gomaxprocs_env=${GOMAXPROCS:-unset}"
  echo "loadavg_at_start=$(uptime)"
  echo "tmpdir=$TMPDIR"
  echo "bin_dir=$RUN_BIN_DIR"
  echo "out_dir=$OUT_DIR"
  echo "disk_free=$(df -h "$OUT_DIR" | tail -1)"
} > "$PASS_DIR/env.txt"

note() {
  echo "--- $*"
  printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >> "$STEPS"
}

# ─── the row table ───────────────────────────────────────────────────────────
#
# One line per RUN, not per example: 24_social_network_cli is six processes and
# each needs its own profile, and 25_software_house_api is a server driven over
# HTTP across its whole lifetime.
#
# Fields, tab-separated:
#   id            row identifier, and the artefact sub-directory name
#   dir           the example directory under examples/
#   timeout       seconds, before TIMEOUT_MULTIPLIER
#   concurrent    yes|no — whether the run does concurrent work, which is what
#                 selects it into the `contention` pass
#   default_argv  argv at the example's own default scale ('-' for none)
#   elevated_argv argv at the elevated scale, with three markers:
#                 '='  argv unchanged; the elevation reaches the row through the
#                      CORPUS the previous row left behind, which is the only
#                      elevation example 24's read-only subcommands have.
#                 '*'  the DEFAULT scale already produces an attributable CPU
#                      profile, so no elevation is applied and the default argv
#                      is used. Measured: 26_social_scale_bench 121.80 s of CPU
#                      and 35_mvcc_mixed_workload 13.31 s, both at their own
#                      defaults. Elevating either would buy nothing the profile
#                      does not already have and would cost the time budget.
#                 '-'  no scale knob at all; skipped by the elevated passes and
#                      reported as a SKIP row rather than omitted.
#
# The elevated values raise the example's PRIMARY SIZE knob and leave its SHAPE
# knobs alone. That distinction was learned the hard way: an earlier round also
# tightened 01_basic's -span from 4000 to 800, which packs the same points into a
# 25x denser space and turns the radius search quadratic. The row ran 11 min 18 s
# at 86% CPU against a 300 s bound instead of the ~2 s the node count predicted.
# Changing one axis at a time is what makes the resulting scale interpretable.
#
# The bounds are deliberately tight (120-300 s). A wrong guess then costs two
# minutes and is reported as a bound expiry with its reason, which is what the
# task asks for, instead of consuming the budget.
rows() {
  cat <<'ROWS'
01_basic	01_basic	120	no	-	-nodes 150000
02_property_graph	02_property_graph	120	no	-	-persons 300000
03_advanced_algorithms	03_advanced_algorithms	240	yes	-	-communities 60 -nodes 800
04_persistence	04_persistence	300	no	-	-packages 40000
05_out_of_core	05_out_of_core	120	no	-	-nodes 300000
06_csv_import	06_csv_import	120	no	-	-nodes 200000
07_graphml_roundtrip	07_graphml_roundtrip	120	no	-	-nodes 150000
08_pagerank	08_pagerank	120	yes	-	-pages 400000
09_leiden	09_leiden	240	yes	-	-communities 80 -community-size 400
10_dimacs9_routing	10_dimacs9_routing	120	no	-	-vertices 300000 -edges 1500000
11_social_network	11_social_network	120	yes	-	-users 100000
12_build_dependency	12_build_dependency	240	no	-	-modules 400000
13_network_reliability	13_network_reliability	120	no	-	-clusters 40 -cluster-size 40
14_routing_alternatives	14_routing_alternatives	300	no	-	-nodes 20000
15_task_assignment	15_task_assignment	240	no	-	-workers 2500 -tasks 2500
16_centrality_analytics	16_centrality_analytics	120	yes	-	-communities 20 -nodes 250
17_transactional_log	17_transactional_log	300	no	-	-accounts 3000 -transfers 9000
18_oocore_pipeline	18_oocore_pipeline	120	yes	-	-nodes 300000
19_pattern_query	19_pattern_query	120	no	-	-nodes 200000
20_concurrent_reads	20_concurrent_reads	120	yes	-	-nodes 60000 -iterations 200
21_typed_recovery	21_typed_recovery	300	no	-	-nodes 50000
22_cypher	22_cypher	120	yes	-	-users 150000
23_bolt_server	23_bolt_server	180	yes	-	-nodes 30000 -queries 60000
24_init	24_social_network_cli	120	no	init -d @DATA@	=
24_seed	24_social_network_cli	180	no	seed -d @DATA@	seed -d @DATA@ -users 150000 -friends 10
24_stats	24_social_network_cli	120	no	stats -d @DATA@	=
24_query	24_social_network_cli	120	no	query -d @DATA@	=
24_plandiff	24_social_network_cli	120	no	plandiff -d @DATA@	=
24_snapshot	24_social_network_cli	180	no	snapshot -d @DATA@	=
25_software_house_api	25_software_house_api	180	yes	-	-scale-components 15000 -scale-tasks 12000 -scale-developers 800
26_social_scale_bench	26_social_scale_bench	300	yes	-	*
27_concurrent_txn	27_concurrent_txn	240	yes	-	-accounts 20000 -ops-per-writer 200
28_negative_weights	28_negative_weights	240	no	-	-layers 80 -width 160
29_all_pairs	29_all_pairs	240	yes	-	-nodes 1600
30_min_spanning_tree	30_min_spanning_tree	240	no	-	-regions 300 -sites 900
31_metrics_observability	31_metrics_observability	120	no	-	-services 80000
32_euler	32_euler	240	no	-	-nodes 300000 -loops 3000
33_generation_swap	33_generation_swap	120	yes	-	-versions 80 -reads-per-reader 2000 -base-nodes 20000
34_bolt_transactions	34_bolt_transactions	240	yes	-	-persons 400000
35_mvcc_mixed_workload	35_mvcc_mixed_workload	120	yes	-	*
36_mvcc_snapshot_topology	36_mvcc_snapshot_topology	360	yes	-	-spokes 4000
37_mvcc_write_contention	37_mvcc_write_contention	120	yes	-	-customers 20000 -ops-per-producer 2000
ROWS
}

# The example-24 data directory is shared across its six rows, in order, because
# each subcommand acts on what the previous one left behind: `stats` on an
# unseeded directory is a different measurement from `stats` on a seeded one.
DATA24="$PASS_DIR/data-24"
QUERY24='MATCH (p:Person)-[:FRIEND]->(q:Person) RETURN p.name, count(q) AS friends ORDER BY friends DESC LIMIT 25'

# ─── build ───────────────────────────────────────────────────────────────────
build_all() {
  local dest="$1"
  shift
  local log="$PASS_DIR/build.log"
  : > "$log"
  local start
  start="$(date +%s)"
  local d name
  for d in "$REPO"/examples/[0-9]*; do
    name="$(basename "$d")"
    go build "$@" -o "$dest/$name" "$MODULE/examples/$name" >> "$log" 2>&1
    local rc=$?
    echo "build_exit[$name]=$rc" >> "$log"
    if [ "$rc" -ne 0 ]; then
      note "BUILD FAILED for $name — see $log"
      return 1
    fi
  done
  echo "build_elapsed_s=$(( $(date +%s) - start ))" >> "$log"
  note "built $(ls -1 "$dest" | wc -l | tr -d ' ') binaries into $dest in $(( $(date +%s) - start ))s"
}

case "$PASS" in
  cover-*)
    if [ ! -x "$COVBIN_DIR/01_basic" ] || [ -n "${LAB_REBUILD:-}" ]; then
      note "building COVERAGE-INSTRUMENTED binaries (-cover -coverpkg=$MODULE/...)"
      build_all "$COVBIN_DIR" -cover "-coverpkg=$MODULE/..." || exit 1
    else
      note "reusing coverage binaries in $COVBIN_DIR"
    fi
    ;;
  *)
    if [ ! -x "$BIN_DIR/01_basic" ] || [ -n "${LAB_REBUILD:-}" ]; then
      note "building PLAIN binaries"
      build_all "$BIN_DIR" || exit 1
    else
      note "reusing plain binaries in $BIN_DIR"
    fi
    ;;
esac

# ─── manifest header ─────────────────────────────────────────────────────────
printf 'pass\trow\texample\tscale\ttimeout_s\texit\telapsed_s\tload1_before\tload1_after\tcpu_ns\tsamples_n\theap_inuse_b\theap_alloc_b\tmutex_delay_ns\tblock_delay_ns\tgoroutines\tartefact_bytes\tverdict\tnotes\targv\n' > "$MANIFEST"

FAILED=0
ROWS_RUN=0
ROWS_SKIPPED=0

load1() { uptime | sed -E 's/.*load averages?: *([0-9.,]+).*/\1/' | tr ',' '.'; }

# run_bounded LOG SECONDS CMD... — run CMD with its output in LOG, and write the
# real exit status INTO LOG as a run_exit= line.
#
# The bound escalates: SIGTERM first, so a program that handles it (the server
# row) can shut down cleanly and still write its profiles, then SIGKILL, which
# nothing can decline. The status recorded for a killed run is the shell's own
# 128+signal convention, and the log also carries an explicit
# bound_expired_after= line so the manifest's reason column is never a guess.
#
# The poll is 5 s. Every bound in the row table is at least an order of
# magnitude larger, so the granularity costs nothing, and a tighter poll would
# put wake-ups on the very host whose loadavg every number here is measured
# against.
run_bounded() {
  local log="$1" secs="$2"
  shift 2
  "$@" > "$log" 2>&1 &
  local pid=$!
  local waited=0
  while [ "$waited" -lt "$secs" ]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 5
    waited=$(( waited + 5 ))
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "bound_expired_after=${secs}s" >> "$log"
    kill -TERM "$pid" 2>/dev/null
    local g=0
    while kill -0 "$pid" 2>/dev/null && [ "$g" -lt 10 ]; do
      sleep 1
      g=$(( g + 1 ))
    done
    kill -KILL "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    echo "run_exit=BOUND" >> "$log"
    return 0
  fi
  wait "$pid"
  echo "run_exit=$?" >> "$log"
}

# probe_total PROBE_FILE BASENAME INDEX — one total, read out of the single probe
# already written for this row. It re-runs nothing: each `go tool pprof` call
# costs real seconds, and there are eight sample indices per row.
probe_total() {
  awk -F'\t' -v b="$2" -v i="$3" '
    { n = $1; sub(/^.*\//, "", n) }
    n == b && $2 == i { print $3; exit }
  ' "$1" 2>/dev/null
}

# run_row — one measured run. Everything it records comes out of files it wrote,
# never out of a pipeline's exit status.
run_row() {
  local id="$1" dir="$2" tmo="$3" concurrent="$4" argv_raw="$5" scale_label="$6"
  local rundir="$PASS_DIR/$id"
  mkdir -p "$rundir" || return 1
  local log="$rundir/run.log"
  local bin="$RUN_BIN_DIR/$dir"

  # Substitute the placeholders the row table uses for paths it cannot know.
  local argv="${argv_raw//@DATA@/$DATA24}"

  local -a cmd
  # shellcheck disable=SC2206 # deliberate word splitting: the row supplies argv.
  if [ "$argv" = "-" ]; then cmd=(); else cmd=($argv); fi
  # The self-test's deliberate breakage. No example defines -lab-self-test-break,
  # so every one of them exits 2 on it, before any profiler starts.
  if [ -n "${LAB_BREAK:-}" ]; then cmd+=(-lab-self-test-break); fi

  # POSITIONAL arguments must come AFTER every flag, including the profiling
  # flags appended below. Go's flag package stops parsing at the first
  # non-flag argument, so `query -d DIR "MATCH ..." -profile-dir X` never sees
  # -profile-dir at all: it reaches the subcommand as a second positional and
  # the row exits 2 with no artefacts. Measured — that is exactly how the
  # 24_query row failed on the first sweep.
  local -a positional=()
  if [ "$id" = "24_query" ]; then positional=("$QUERY24"); fi

  local -a prof=(-profile-dir "$rundir")
  case "$PASS" in
    default | contention) prof+=(-contention 1) ;;
  esac
  case "$PASS" in
    cover-*)
      prof=()
      export GOCOVERDIR="$COVDIR"
      ;;
  esac

  local lb la t0 t1 rc elapsed
  lb="$(load1)"
  uptime > "$rundir/loadavg-before.txt"
  t0="$(date +%s)"

  if [ "$id" = "25_software_house_api" ]; then
    drive_server "$bin" "$rundir" "$log" "$(( tmo * TMUL ))" "${cmd[@]+"${cmd[@]}"}" "${prof[@]+"${prof[@]}"}"
  else
    (
      cd "$rundir" || exit 111
      run_bounded "$log" "$(( tmo * TMUL ))" \
        "$bin" "${cmd[@]+"${cmd[@]}"}" "${prof[@]+"${prof[@]}"}" \
        "${positional[@]+"${positional[@]}"}"
    )
  fi

  t1="$(date +%s)"
  elapsed=$(( t1 - t0 ))
  uptime > "$rundir/loadavg-after.txt"
  la="$(load1)"

  # THE EXIT STATUS IS READ OUT OF THE LOG, never out of the subshell. A
  # pipeline's or a wrapper's status has lied on this project more than once.
  rc="$(sed -n 's/^run_exit=//p' "$log" | tail -1)"
  [ -n "$rc" ] || rc="NOSTATUS"

  # ─── artefact probe ────────────────────────────────────────────────────────
  python3 "$PROBE" "$rundir"/*.pprof > "$rundir/probe.txt" 2>&1
  echo "probe_exit=$?" >> "$rundir/probe.txt"

  local pf="$rundir/probe.txt"
  local cpu samples inuse alloc mdelay bdelay grout
  cpu="$(probe_total "$pf" cpu.pprof cpu)"
  samples="$(probe_total "$pf" cpu.pprof samples)"
  inuse="$(probe_total "$pf" heap.pprof inuse_space)"
  alloc="$(probe_total "$pf" heap.pprof alloc_space)"
  mdelay="$(probe_total "$pf" mutex.pprof delay)"
  bdelay="$(probe_total "$pf" block.pprof delay)"
  grout="$(probe_total "$pf" goroutine.pprof goroutine)"

  local bytes
  bytes="$(ls -l "$rundir"/*.pprof 2>/dev/null | awk '{s+=$5} END {print s+0}')"

  # ─── verdict ───────────────────────────────────────────────────────────────
  #
  # A row FAILS when it produced no profile, or a profile whose total is zero.
  # Both are asserted on the numbers the probe read out of the artefacts, not on
  # the presence of a file: runtime/pprof writes a well-formed empty profile
  # quite happily.
  local verdict="PASS" notes=""
  case "$PASS" in
    cover-*)
      # No profile is asked for, so the artefact assertion is the counter file.
      if [ -z "$(ls -A "$COVDIR" 2>/dev/null)" ]; then
        verdict="FAIL"
        notes="no coverage counters"
      fi
      ;;
    *)
      if [ ! -s "$rundir/cpu.pprof" ]; then
        verdict="FAIL"
        notes="${notes}no-cpu-profile;"
      elif [ -z "$cpu" ] || [ "$cpu" = "0" ]; then
        verdict="FAIL"
        notes="${notes}cpu-total-zero;"
      fi
      if [ ! -s "$rundir/heap.pprof" ]; then
        verdict="FAIL"
        notes="${notes}no-heap-profile;"
      elif [ -z "$alloc" ] || [ "$alloc" = "0" ]; then
        verdict="FAIL"
        notes="${notes}heap-alloc-zero;"
      fi
      ;;
  esac
  case "$PASS" in
    default | contention)
      for p in mutex block goroutine; do
        if [ ! -s "$rundir/$p.pprof" ]; then
          verdict="FAIL"
          notes="${notes}no-$p-profile;"
        fi
      done
      ;;
  esac
  if [ "$rc" != "0" ]; then
    notes="${notes}exit=$rc;"
    if [ "$rc" = "BOUND" ]; then
      notes="${notes}BOUND-EXPIRED-after-$(sed -n 's/^bound_expired_after=//p' "$log" | tail -1);"
    fi
    verdict="FAIL"
  fi

  [ "$verdict" = "FAIL" ] && FAILED=$(( FAILED + 1 ))
  ROWS_RUN=$(( ROWS_RUN + 1 ))

  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$PASS" "$id" "$dir" "$scale_label" "$(( tmo * TMUL ))" "$rc" "$elapsed" \
    "$lb" "$la" "${cpu:--}" "${samples:--}" "${inuse:--}" "${alloc:--}" \
    "${mdelay:--}" "${bdelay:--}" "${grout:--}" "$bytes" "$verdict" \
    "${notes:--}" \
    "$bin ${cmd[*]+${cmd[*]}} ${prof[*]+${prof[*]}} ${positional[*]+${positional[*]}}" \
    >> "$MANIFEST"

  note "$id [$scale_label] exit=$rc ${elapsed}s load $lb->$la cpu=${cpu:--}ns $verdict ${notes:-}"
}

# drive_server — example 25 is the one row that is a server. It is profiled
# across its whole lifetime — startup recovery, the synthetic seed, the request
# battery and the graceful shutdown — because for a server those are the phases
# worth attributing. The request battery is what makes the profile non-trivial;
# a server that only starts and stops measures its own startup.
drive_server() {
  local bin="$1" rundir="$2" log="$3" tmo="$4"
  shift 4
  local addr=":18025"
  local data="$rundir/data"
  mkdir -p "$data"
  (
    cd "$rundir" || exit 111
    run_bounded "$log" "$tmo" "$bin" -d "$data" -addr "$addr" "$@"
  ) &
  local runner=$!
  echo "$runner" > "$rundir/server.pid"

  # Wait for the listener, bounded. A fixed sleep would either race the seed or
  # waste the budget.
  local i
  for i in $(seq 1 120); do
    if curl -s -o /dev/null --max-time 2 "http://localhost${addr}/stats"; then break; fi
    sleep 1
  done

  local reqs=0
  {
    for i in $(seq 1 400); do
      curl -s -o /dev/null --max-time 10 "http://localhost${addr}/stats"
      curl -s -o /dev/null --max-time 10 "http://localhost${addr}/schema"
      curl -s -o /dev/null --max-time 10 -XPOST "http://localhost${addr}/query" \
        -d '{"cypher":"MATCH (t:Task) RETURN count(t) AS n"}'
      reqs=$(( reqs + 3 ))
    done
    echo "http_requests=$reqs"
  } > "$rundir/http.log" 2>&1

  # SIGTERM, then wait for the process to write its own exit status. The runner
  # is `timeout`, so signal the whole process group's example binary by name
  # under this run's directory only.
  pkill -TERM -f "$bin -d $data" 2>/dev/null
  wait "$runner" 2>/dev/null
  rm -f "$rundir/server.pid"
}

# ─── the sweep ───────────────────────────────────────────────────────────────
note "pass=$PASS out=$PASS_DIR loadavg=$(load1)"
mkdir -p "$DATA24"

while IFS=$'\t' read -r id dir tmo concurrent dargv eargv; do
  [ -n "${id:-}" ] || continue
  # bash's own =~, deliberately, not grep: grep on this host is ugrep and has
  # been observed returning silent empty output, which here would silently drop
  # every row instead of filtering them.
  if [ -n "${LAB_ONLY:-}" ]; then
    [[ "$id" =~ $LAB_ONLY ]] || continue
  fi
  case "$PASS" in
    default | cover-default)
      run_row "$id" "$dir" "$tmo" "$concurrent" "$dargv" "default"
      ;;
    elevated | cover-elevated)
      if [ "$eargv" = "*" ]; then
        run_row "$id" "$dir" "$tmo" "$concurrent" "$dargv" "default-already-over-2s"
        continue
      fi
      if [ "$eargv" = "=" ]; then
        run_row "$id" "$dir" "$tmo" "$concurrent" "$dargv" "elevated-corpus"
        continue
      fi
      if [ "$eargv" = "-" ]; then
        ROWS_SKIPPED=$(( ROWS_SKIPPED + 1 ))
        printf '%s\t%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\t%s\t-\n' \
          "$PASS" "$id" "$dir" "no-scale-knob" "SKIP" \
          "no scale knob: the run is fixed by what the previous row left behind" >> "$MANIFEST"
        note "$id SKIP — no scale knob"
        continue
      fi
      run_row "$id" "$dir" "$tmo" "$concurrent" "$eargv" "elevated"
      ;;
    contention)
      if [ "$concurrent" != "yes" ]; then
        ROWS_SKIPPED=$(( ROWS_SKIPPED + 1 ))
        note "$id SKIP — not concurrent"
        continue
      fi
      if [ "$eargv" = "*" ]; then
        run_row "$id" "$dir" "$tmo" "$concurrent" "$dargv" "default-already-over-2s"
      elif [ "$eargv" = "=" ]; then
        run_row "$id" "$dir" "$tmo" "$concurrent" "$dargv" "elevated-corpus"
      elif [ "$eargv" = "-" ]; then
        ROWS_SKIPPED=$(( ROWS_SKIPPED + 1 ))
        note "$id SKIP — concurrent but no scale knob"
      else
        run_row "$id" "$dir" "$tmo" "$concurrent" "$eargv" "elevated"
      fi
      ;;
  esac
done < <(rows)

{
  echo "rows_run=$ROWS_RUN"
  echo "rows_skipped=$ROWS_SKIPPED"
  echo "rows_failed=$FAILED"
  echo "loadavg_at_end=$(uptime)"
  echo "disk_used_bytes=$(du -sk "$PASS_DIR" | awk '{print $1*1024}')"
  echo "tmpdir_left_bytes=$(du -sk "$TMPDIR" 2>/dev/null | awk '{print $1*1024}')"
} >> "$PASS_DIR/env.txt"

note "rows_run=$ROWS_RUN rows_skipped=$ROWS_SKIPPED rows_failed=$FAILED"
echo "manifest: $MANIFEST"

# A sweep that ran nothing is a FAILED sweep, not an empty one. This guard is
# here because it was needed: a single mis-typed function call in the row loop
# made the whole pass run zero rows and still exit 0, and only the self-test
# noticed. An exit status of 0 must mean "every row measured", never "nothing
# was attempted".
if [ "$ROWS_RUN" -eq 0 ]; then
  echo "NO ROWS RAN — see $STEPS and $PASS_DIR/env.txt" >&2
  exit 1
fi
[ "$FAILED" -eq 0 ]
