#!/bin/bash
# size.sh — ONE invocation of every block on arm B, timed, so the campaign's round
# count is chosen from a measured cost and not a guessed one.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad
source $SP/work/blocks.sh
OUT=$SP/work/sizing; mkdir -p $OUT
LOG=$OUT/sizing.log; : > "$LOG"
echo "START $(date -u +%FT%TZ) load=[$(uptime | sed 's/.*averages*: //')]" >> "$LOG"
for spec in "${BLOCKS[@]}"; do
  IFS='#' read -r label stem sel ladder <<< "$spec"
  cpu=""; [ "$ladder" = y ] && cpu="-test.cpu=1,8,64,256,1024"
  echo "block=$label BEFORE $(uptime)" >> "$LOG"
  t0=$(date +%s)
  $SP/bin/${stem}_B.test -test.run=XXXNONE -test.bench="$sel" -test.benchmem -test.count=1 $cpu \
      > "$OUT/${label}.txt" 2> "$OUT/${label}.stderr"
  ec=$?
  t1=$(date +%s)
  n=$(grep -cE '^Benchmark\S*[[:space:]]+[0-9]+[[:space:]]+' "$OUT/${label}.txt")
  echo "block=$label EXIT=$ec SECONDS=$((t1-t0)) RESULTLINES=$n" >> "$LOG"
  echo "block=$label AFTER  $(uptime)" >> "$LOG"
done
echo "END $(date -u +%FT%TZ)" >> "$LOG"
echo "SIZING_ALLDONE" >> "$LOG"
