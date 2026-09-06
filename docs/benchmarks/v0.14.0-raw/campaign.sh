#!/bin/bash
# campaign.sh — the interleaved A/B/B2 measurement campaign for docs/benchmarks/v0.14.0.md
#
# Arms
#   A  = v0.13.0  (b439283e)
#   B  = v0.14.0  (f8a3ac0b)
#   B2 = a SECOND independent build of the SAME v0.14.0 tree. Verified byte-identical
#        to B by sha256 for all 11 packages, so B-vs-B2 is the SAME BINARY AGAINST
#        ITSELF and therefore a pure host-noise floor -- measured in the SAME ROUNDS,
#        under the SAME load, as the signal it is the yardstick for.
#
# Four packages (graph, graph/mvcc, internal/metrics/prometheus, store/wal) compile to
# binaries that are byte-identical BETWEEN THE RELEASES too. On those, A-vs-B is a
# second floor, measured ACROSS the arms -- it catches anything the within-arm floor
# structurally cannot.
#
# Discipline, each rule paid for by a past defect on this project:
#   * stderr to its OWN file -- never 2>&1; that splice destroyed result lines before.
#   * exit code appended to exits.log and read from THERE, never from a pipeline.
#   * uptime bracketed BEFORE and AFTER every single invocation.
#   * arm order rotated every round, so no arm systematically inherits another's
#     cache and thermal aftermath.
#   * results mirrored into the repo after every round, so an interrupted run still
#     leaves everything it had already measured on disk.
#   * a load gate before each round, with the gate value recorded.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad
source $SP/work/blocks.sh
OUT=$SP/raw/campaign
MIRROR=/Users/flaviocfo/dev/xumiga/GoGraph/docs/benchmarks/v0.14.0-raw
ROUNDS=${ROUNDS:-6}
GATE=${GATE:-2.5}
mkdir -p "$OUT" "$MIRROR"

rm -f "$OUT"/*.txt "$OUT"/*.stderr      # a re-run must not append to a previous run's samples
EXITLOG=$OUT/exits.log
LOADLOG=$OUT/loadavg.log
PROGRESS=$OUT/progress.log
: > "$EXITLOG"; : > "$LOADLOG"; : > "$PROGRESS"

# Locale-proof: this host is pt_PT, uptime prints "10,01", and awk then compares
# "10.01" > "2.0" as STRINGS and silently returns false. See work/load1.py.
load1() { python3 $SP/work/load1.py; }
loads() { uptime | sed 's/.*averages*: //' | tr ',' '.'; }
busy() { python3 -c "import os,sys; sys.exit(0 if os.getloadavg()[0] > $GATE else 1)"; }

echo "START $(date -u +%FT%TZ) rounds=$ROUNDS gate=$GATE go=$(go version)" | tee -a "$PROGRESS" >> "$EXITLOG"

for r in $(seq 1 "$ROUNDS"); do
  # --- load gate: wait (bounded) for a quiet host, and RECORD what we opened at ---
  waited=0
  while busy && [ "$waited" -lt 600 ]; do
    sleep 20; waited=$((waited+20))
  done
  echo "ROUND $r GATE_OPEN load1=$(load1) waited=${waited}s $(date -u +%FT%TZ)" | tee -a "$PROGRESS" >> "$EXITLOG"

  case $(( (r - 1) % 3 )) in
    0) ARMS="A B B2" ;;
    1) ARMS="B B2 A" ;;
    2) ARMS="B2 A B" ;;
  esac
  echo "ROUND $r START $(date -u +%FT%TZ) order=[$ARMS] load=[$(loads)]" >> "$PROGRESS"

  for spec in "${BLOCKS[@]}"; do
    IFS='#' read -r label stem sel ladder <<< "$spec"
    cpu=""; [ "$ladder" = y ] && cpu="-test.cpu=1,8,64,256,1024"
    for arm in $ARMS; do
      echo "round=$r block=$label arm=$arm BEFORE $(uptime)" >> "$LOADLOG"
      t0=$(date +%s)
      "$SP/bin/${stem}_${arm}.test" \
          -test.run=XXXNONE -test.bench="$sel" -test.benchmem -test.count=1 $cpu \
        >> "$OUT/${label}_${arm}.txt" 2>> "$OUT/${label}_${arm}.stderr"
      ec=$?
      t1=$(date +%s)
      echo "round=$r block=$label arm=$arm EXIT=$ec SECONDS=$((t1-t0))" >> "$EXITLOG"
      echo "round=$r block=$label arm=$arm AFTER  $(uptime)" >> "$LOADLOG"
    done
  done

  echo "ROUND $r COMPLETE $(date -u +%FT%TZ) load=[$(loads)]" | tee -a "$PROGRESS" >> "$EXITLOG"
  cp "$OUT"/*.txt "$OUT"/*.stderr "$EXITLOG" "$LOADLOG" "$PROGRESS" "$MIRROR"/ 2>/dev/null
  echo "ROUND $r MIRRORED $(date -u +%FT%TZ)" >> "$PROGRESS"
done

echo "END $(date -u +%FT%TZ) load=[$(loads)]" | tee -a "$PROGRESS" >> "$EXITLOG"
echo "CAMPAIGN_ALLDONE" >> "$PROGRESS"
