#!/bin/bash
# size.sh — ONE invocation of every block on arm B, timed on a QUIET host, so the
# campaign's round count and block set are chosen from a MEASURED cost rather than a
# guessed one — and so a per-block cost is on record for the next campaign.
#
# Run this ONLY when nothing else is running. A previous sizing pass on this release
# was taken while doc-audit subagents and Spotlight were live and had to be discarded
# (see work/sizing-DISCARDED-contaminated/WHY-DISCARDED.txt).
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad
source "$SP/work/blocks.sh"
OUT=$SP/work/sizing; mkdir -p "$OUT"
LOG=$OUT/sizing.log; : > "$LOG"
SAMPLES=$OUT/loadsamples.txt; : > "$SAMPLES"
hot() { ps -eo pcpu,comm | sort -rn | awk '$1>5 {printf "%s:%s ", $2, $1}' | head -c 400; }

python3 "$SP/work/loadsampler.py" "$SAMPLES" 5 &
SAMPLER_PID=$!
cleanup() {
  kill "$SAMPLER_PID" 2>/dev/null
  if kill -0 "$SAMPLER_PID" 2>/dev/null; then echo "SAMPLER STILL ALIVE pid=$SAMPLER_PID" >> "$LOG"
  else echo "SAMPLER STOPPED pid=$SAMPLER_PID samples=$(wc -l < "$SAMPLES")" >> "$LOG"; fi
}
trap cleanup EXIT

echo "START $(date -u +%FT%TZ) load1=$(python3 "$SP/work/load1.py") hot=[$(hot)]" >> "$LOG"
for spec in "${BLOCKS[@]}"; do
  IFS='#' read -r label stem sel ladder arms <<< "$spec"
  cpu=""; [ "$ladder" = y ] && cpu="-test.cpu=1,8,64,256,1024"
  echo "block=$label BEFORE $(uptime) hot=[$(hot)]" >> "$LOG"
  t0=$(python3 -c 'import time;print(f"{time.time():.3f}")'); s0=$(date +%s)
  "$SP/bin/${stem}_B.test" -test.run=XXXNONE -test.bench="$sel" -test.benchmem -test.count=1 $cpu \
      > "$OUT/${label}.txt" 2> "$OUT/${label}.stderr"
  ec=$?
  t1=$(python3 -c 'import time;print(f"{time.time():.3f}")'); s1=$(date +%s)
  n=$(grep -cE '^Benchmark\S*[[:space:]]+[0-9]+[[:space:]]+' "$OUT/${label}.txt")
  echo "round=0 block=$label arm=B EXIT=$ec SECONDS=$((s1-s0)) T0=$t0 T1=$t1 RESULTLINES=$n" >> "$LOG"
  echo "block=$label AFTER  $(uptime) hot=[$(hot)]" >> "$LOG"
done
echo "END $(date -u +%FT%TZ)" >> "$LOG"
echo "SIZING_ALLDONE" >> "$LOG"
