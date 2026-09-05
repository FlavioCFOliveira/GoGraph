#!/bin/bash
# rmp2762-parallel-dbhits-ab.sh — interleaved A/B/C benchmark driver for rmp #2762.
#
# Three arms, run in strict rotation within each round so thermal drift and
# background activity bias all three equally:
#   base  — worktree at HEAD (e6f6384b), the pre-#2762 code
#   base2 — a byte-identical copy of base, built separately: the NOISE FLOOR
#   head  — the working tree with the per-morsel storage counter
#
# loadavg is recorded before every single invocation, so no number can later be
# called "idle" without the evidence being in the log.
set -u
SP="$(cd "$(dirname "$0")" && pwd)"
HEAD_DIR=/Users/flaviocfo/dev/xumiga/GoGraph
OUT="$SP/ab"
mkdir -p "$OUT"

BENCHA='BenchmarkParallelScan_CountBig|BenchmarkParallelScanProject_Scan(Big|FilterBig)|BenchmarkParallelAggregate_(MinBig|GroupMinBig)|BenchmarkParallelLabelScan_LabelledProject'
BENCHB='BenchmarkParallelAggregate_Concurrent'

run_one() {  # $1=arm dir  $2=arm name  $3=set name  $4=bench regex  $5=cpu flag  $6=round
  echo "### round=$6 arm=$2 set=$3 loadavg=$(uptime | sed 's/.*load averages*: //')" >> "$OUT/loadavg.log"
  ( cd "$1" && go test -run='^$' -bench="$4" -benchmem -count=1 $5 ./cypher/ ) \
      >> "$OUT/$3.$2.txt" 2>&1
  echo "### round=$6 arm=$2 set=$3 exit=$? " >> "$OUT/exits.log"
}

: > "$OUT/loadavg.log"
: > "$OUT/exits.log"
for f in A B; do for a in base base2 head; do : > "$OUT/$f.$a.txt"; done; done

for r in 1 2 3 4 5; do
  run_one "$SP/base"  base  A "$BENCHA" "-cpu=1,4,10" "$r"
  run_one "$SP/base2" base2 A "$BENCHA" "-cpu=1,4,10" "$r"
  run_one "$HEAD_DIR" head  A "$BENCHA" "-cpu=1,4,10" "$r"
done

for r in 1 2 3 4 5 6 7 8; do
  run_one "$SP/base"  base  B "$BENCHB" "" "$r"
  run_one "$SP/base2" base2 B "$BENCHB" "" "$r"
  run_one "$HEAD_DIR" head  B "$BENCHB" "" "$r"
done

echo "ALL_DONE" >> "$OUT/exits.log"
