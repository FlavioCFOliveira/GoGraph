#!/bin/bash
# campaign.sh — the interleaved A/B/B2 measurement campaign for docs/benchmarks/v0.14.1.md
#
# Arms
#   A  = v0.14.0  (tag v0.14.0 -> commit 7c59a02c)
#   B  = v0.14.1 candidate (efd32fb9)
#   B2 = a SECOND independent compilation of the SAME candidate tree. Verified
#        byte-identical to B by sha256 for ALL 18 packages, so B-vs-B2 is the SAME
#        BINARY AGAINST ITSELF and therefore a pure host-noise floor -- measured in
#        the SAME ROUNDS, under the SAME load, as the signal it is the yardstick for.
#
# Four packages (graph, graph/index/count, graph/mvcc, internal/metrics/prometheus)
# compile to binaries that are byte-identical BETWEEN the releases too. On those,
# A-vs-B is a SECOND floor, measured ACROSS the arms.
#
# Discipline, each rule paid for by a past defect on this project:
#   * NOTHING ELSE RUNS. No subagent, no sweep, no grep, no build, no repo write
#     inside a timing. Uncontrolled VARYING load is the one thing an interleaved
#     A/B cannot absorb -- cypher/exec has measured 274.6s against 25.9s on an
#     IDENTICAL commit under contention, and Spotlight indexing the arms once
#     contaminated a whole campaign.
#   * A LOAD SAMPLER RUNS FOR THE WHOLE CAMPAIGN, and every invocation records its
#     own T0/T1 so the load DURING it can be sliced out. Before/after brackets
#     cannot see a spike that starts and ends inside an invocation.
#   * stderr to its OWN file -- never 2>&1; that splice destroyed result lines before.
#   * exit code appended to exits.log and read from THERE, never from a pipeline.
#   * arm order rotated every round, so no arm systematically inherits another's
#     cache and thermal aftermath.
#   * results mirrored into the repo AFTER a round and BEFORE the next round's gate,
#     so the gate absorbs whatever indexing the write provokes.
#   * a load gate before each round, with the value it opened at recorded.
#   * the gate reads os.getloadavg() and has no text step: this host's locale is
#     pt_PT, uptime prints "10,01", and a shell gate compares "10.01" > "2.5" as
#     STRINGS and silently never fires.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad
source "$SP/work/blocks.sh"
OUT=$SP/raw/campaign
MIRROR=/Users/flaviocfo/dev/xumiga/GoGraph/docs/benchmarks/v0.14.1-raw
ROUNDS=${ROUNDS:-6}
GATE=${GATE:-2.5}
SAMPLE_INTERVAL=${SAMPLE_INTERVAL:-2}
mkdir -p "$OUT" "$MIRROR"

rm -f "$OUT"/*.txt "$OUT"/*.stderr      # a re-run must not append to a previous run's samples
EXITLOG=$OUT/exits.log
LOADLOG=$OUT/loadavg.log
SAMPLES=$OUT/loadsamples.txt
PROGRESS=$OUT/progress.log
: > "$EXITLOG"; : > "$LOADLOG"; : > "$SAMPLES"; : > "$PROGRESS"

load1() { python3 "$SP/work/load1.py"; }
loads() { uptime | sed 's/.*averages*: //'; }
busy()  { python3 -c "import os,sys; sys.exit(0 if os.getloadavg()[0] > $GATE else 1)"; }
# hot() names the non-GoGraph processes above 5% CPU, so a contaminated window is
# attributable afterwards instead of merely suspected. Spotlight is called out by
# name because it is the one that has already ruined a campaign here.
hot()   { ps -eo pcpu,comm | sort -rn | awk '$1>5 {printf "%s:%s ", $2, $1}' | head -c 400; }

# --- the sampler: ONE process for the whole campaign -------------------------
python3 "$SP/work/loadsampler.py" "$SAMPLES" "$SAMPLE_INTERVAL" &
SAMPLER_PID=$!
echo "$SAMPLER_PID" > "$OUT/sampler.pid"
cleanup() {
  # Verify the cleanup ran rather than assuming it did -- and give the kill time
  # to land before checking. The sizing run's trap checked kill -0 immediately
  # after kill and reported "STILL ALIVE" for a process that was already dead:
  # a FALSE NEGATIVE in the verification, which is as bad as no verification,
  # because it would have had me hunting a leak that did not exist. Poll with a
  # bounded wait, then escalate.
  kill -TERM "$SAMPLER_PID" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$SAMPLER_PID" 2>/dev/null || break
    sleep 0.3
  done
  if kill -0 "$SAMPLER_PID" 2>/dev/null; then
    kill -9 "$SAMPLER_PID" 2>/dev/null
    sleep 0.5
  fi
  if kill -0 "$SAMPLER_PID" 2>/dev/null; then
    echo "SAMPLER STILL ALIVE AFTER SIGKILL pid=$SAMPLER_PID" >> "$PROGRESS"
  else
    echo "SAMPLER STOPPED pid=$SAMPLER_PID samples=$(wc -l < "$SAMPLES")" >> "$PROGRESS"
  fi
}
trap cleanup EXIT

echo "START $(date -u +%FT%TZ) rounds=$ROUNDS gate=$GATE sample=${SAMPLE_INTERVAL}s go=$(go version)" \
  | tee -a "$PROGRESS" >> "$EXITLOG"
echo "PRE-CAMPAIGN load1=$(load1) hot=[$(hot)]" | tee -a "$PROGRESS" >> "$EXITLOG"

for r in $(seq 1 "$ROUNDS"); do
  waited=0
  while busy && [ "$waited" -lt 600 ]; do
    sleep 20; waited=$((waited+20))
  done
  echo "ROUND $r GATE_OPEN load1=$(load1) waited=${waited}s hot=[$(hot)] $(date -u +%FT%TZ)" \
    | tee -a "$PROGRESS" >> "$EXITLOG"

  case $(( (r - 1) % 3 )) in
    0) ARMS="A B B2" ;;
    1) ARMS="B B2 A" ;;
    2) ARMS="B2 A B" ;;
  esac
  echo "ROUND $r START $(date -u +%FT%TZ) order=[$ARMS] load=[$(loads)]" >> "$PROGRESS"

  for spec in "${BLOCKS[@]}"; do
    IFS='#' read -r label stem sel ladder arms <<< "$spec"
    cpu=""; [ "$ladder" = y ] && cpu="-test.cpu=1,8,64,256,1024"
    for arm in $ARMS; do
      [ "$arms" = BB2 ] && [ "$arm" = A ] && continue
      echo "round=$r block=$label arm=$arm BEFORE $(uptime) hot=[$(hot)]" >> "$LOADLOG"
      t0=$(python3 -c 'import time;print(f"{time.time():.3f}")')
      s0=$(date +%s)
      "$SP/bin/${stem}_${arm}.test" \
          -test.run=XXXNONE -test.bench="$sel" -test.benchmem -test.count=1 $cpu \
        >> "$OUT/${label}_${arm}.txt" 2>> "$OUT/${label}_${arm}.stderr"
      ec=$?
      t1=$(python3 -c 'import time;print(f"{time.time():.3f}")')
      s1=$(date +%s)
      echo "round=$r block=$label arm=$arm EXIT=$ec SECONDS=$((s1-s0)) T0=$t0 T1=$t1" >> "$EXITLOG"
      echo "round=$r block=$label arm=$arm AFTER  $(uptime) hot=[$(hot)]" >> "$LOADLOG"
    done
  done

  echo "ROUND $r COMPLETE $(date -u +%FT%TZ) load=[$(loads)]" | tee -a "$PROGRESS" >> "$EXITLOG"
  cp "$OUT"/*.txt "$OUT"/*.stderr "$EXITLOG" "$LOADLOG" "$SAMPLES" "$PROGRESS" "$MIRROR"/ 2>/dev/null
  echo "ROUND $r MIRRORED $(date -u +%FT%TZ)" >> "$PROGRESS"
done

echo "END $(date -u +%FT%TZ) load=[$(loads)] hot=[$(hot)]" | tee -a "$PROGRESS" >> "$EXITLOG"
echo "CAMPAIGN_ALLDONE" >> "$PROGRESS"
