#!/bin/bash
# run_ab2.sh — REDUCED-SCOPE interleaved A/B/B2 rounds, v0.13.0 vs v0.14.0.
#
# Scope was cut after the first attempt proved too long to finish. What is kept and
# why (the dropped surfaces are named in the report):
#   cypher_exec  Expand (#2761) + ShortestPath/AllShortestPaths (#2763) + Filter
#                (#2764) + Scan/Project/Drain/Limit as in-package controls
#   cypher       the three parallel leaves (#2762), plan-cache hits (#2765), the
#                count/label-count path (#2771), an engine read path, the WRITE path
#                as a control, and the ported reorder head-to-head (#2766/#2771)
#   store_wal    test binary is BYTE-IDENTICAL between the two versions, so this is
#                a second noise floor measured ACROSS the arms
#   store_txn    commit path — a control that must not move
# Dropped: graph/lpg (adds breadth, not evidence), the engine-level ColumnarShape,
# IndexIntersectPlan, MergeMatch and ExpandInto families.
#
# Arms: A = v0.13.0, B = v0.14.0, B2 = a second independent build of the SAME
# v0.14.0 tree (byte-identical to B by shasum), so B vs B2 is the same binary
# against itself — the pure host-noise floor, measured under the SAME conditions and
# in the SAME rounds as the signal.
#
# Discipline: stderr to its own file (never 2>&1 — that splice destroyed 4 of 6
# result lines in sizing); uptime bracketed before AND after every invocation; exit
# code appended to the log and read from there; arm order rotated every round;
# results mirrored into the repo after every round so progress is on disk.

set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad
OUT=$SP/raw/ab2
MIRROR=/Users/flaviocfo/dev/xumiga/GoGraph/docs/benchmarks/v0130-vs-v0140-2026-09-06-raw
mkdir -p "$OUT" "$MIRROR"

ROUNDS=8

EXEC='BenchmarkExpand|BenchmarkShortestPath|BenchmarkAllShortestPaths|BenchmarkFilter_Throughput|BenchmarkProject_Throughput|BenchmarkScan_PerNode|BenchmarkAllNodesScan_PerNodeAllocCost|BenchmarkDrain_Throughput|BenchmarkLimit_Throughput'
CY='BenchmarkParallelScan_|BenchmarkParallelScanProject_|BenchmarkParallelLabelScan_|BenchmarkPlanCacheHit_|BenchmarkPlanCacheMiss_|BenchmarkCount_Label|BenchmarkCountPushdown_|BenchmarkCountAllNodes|BenchmarkReadOnly_|BenchmarkCreateRelationships|BenchmarkDeleteAccumulated|BenchmarkZZReorder'
WAL='BenchmarkCRCIncremental|BenchmarkEncode|BenchmarkDecode|BenchmarkReader_Replay'
TXN='BenchmarkCommit$|BenchmarkCommitConcurrent$'

PKGS="cypher_exec cypher store_wal store_txn"

sel_for() {
  case "$1" in
    cypher_exec) printf '%s' "$EXEC" ;;
    cypher)      printf '%s' "$CY" ;;
    store_wal)   printf '%s' "$WAL" ;;
    store_txn)   printf '%s' "$TXN" ;;
  esac
}

EXITLOG=$OUT/exits.log
LOADLOG=$OUT/loadavg.log
PROGRESS=$OUT/progress.log
: > "$EXITLOG"; : > "$LOADLOG"; : > "$PROGRESS"

echo "START $(date -u +%Y-%m-%dT%H:%M:%SZ) rounds=$ROUNDS" | tee -a "$PROGRESS" >> "$EXITLOG"

for r in $(seq 1 $ROUNDS); do
  case $(( (r - 1) % 3 )) in
    0) ARMS="A B B2" ;;
    1) ARMS="B B2 A" ;;
    2) ARMS="B2 A B" ;;
  esac
  echo "ROUND $r START $(date -u +%Y-%m-%dT%H:%M:%SZ) order=[$ARMS] load=[$(uptime | sed 's/.*averages*: //')]" >> "$PROGRESS"
  for pkg in $PKGS; do
    sel=$(sel_for "$pkg")
    for arm in $ARMS; do
      echo "round=$r pkg=$pkg arm=$arm BEFORE $(uptime)" >> "$LOADLOG"
      "$SP/bin/${pkg}_${arm}.test" \
          -test.run=XXXNONE -test.bench="$sel" -test.benchmem -test.count=1 \
        >> "$OUT/${pkg}_${arm}.txt" 2>> "$OUT/${pkg}_${arm}.stderr"
      ec=$?
      echo "round=$r pkg=$pkg arm=$arm EXIT=$ec" >> "$EXITLOG"
      echo "round=$r pkg=$pkg arm=$arm AFTER  $(uptime)" >> "$LOADLOG"
    done
  done
  echo "ROUND $r COMPLETE $(date -u +%Y-%m-%dT%H:%M:%SZ) load=[$(uptime | sed 's/.*averages*: //')]" | tee -a "$PROGRESS" >> "$EXITLOG"
  # Mirror to the repo after every round, so progress is on disk and inspectable.
  cp "$OUT"/*.txt "$OUT"/exits.log "$OUT"/loadavg.log "$OUT"/progress.log "$MIRROR"/ 2>/dev/null
done

echo "END $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$PROGRESS" >> "$EXITLOG"
echo "ALLDONE" >> "$PROGRESS"
