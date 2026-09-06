#!/bin/bash
# run_ab.sh — interleaved A/B/B2 rounds for the v0.13.0 vs v0.14.0 comparison.
#
#   A  = v0.13.0 test binary
#   B  = v0.14.0 test binary
#   B2 = a SECOND, independently produced build of the SAME v0.14.0 tree.
#        `go build` is deterministic here, so B2 is byte-identical to B (proved by
#        shasum in binary_sha256_final.txt) — which makes B vs B2 the same binary
#        against itself, i.e. the pure host-noise floor.
#
# Discipline enforced here:
#   * stderr is written to its OWN file, never spliced into the result stream with
#     2>&1 — the engine logs a WARN on construction and that splice destroyed 4 of 6
#     result lines in the sizing run;
#   * `uptime` is captured immediately BEFORE and immediately AFTER every single
#     invocation, so external load cannot be silently included in a window;
#   * the exit code is appended to the run's own log and read from THERE, never from
#     a pipeline's exit status;
#   * the arm order ROTATES every round, so no arm is systematically first and
#     thermal drift biases all three equally.

set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad
OUT=$SP/raw/ab
mkdir -p "$OUT"

ROUNDS=8

CY='BenchmarkParallelScan_|BenchmarkParallelScanProject_|BenchmarkParallelLabelScan_|BenchmarkParallelScanGate|BenchmarkParallelAggregate_(Min|Max|Group)|BenchmarkColumnarShape_|BenchmarkPlanCacheHit_|BenchmarkPlanCacheMiss_|BenchmarkPlanReusePhases|BenchmarkCount_Label|BenchmarkCountPushdown_|BenchmarkCountAllNodes|BenchmarkExpandInto_(TriangleClose|TwoHopOpen|TwoCycleClose)|BenchmarkReadOnly_|BenchmarkReadAfterWrite_|BenchmarkCreateRelationships|BenchmarkDeleteAccumulated|BenchmarkMergeMatch_LabelsOnly|BenchmarkZZReorder'
WAL='BenchmarkCRCIncremental|BenchmarkEncode|BenchmarkDecode|BenchmarkReader_Replay'
TXN='BenchmarkCommit$|BenchmarkCommitConcurrent$'
LPG='BenchmarkOutDegree_ByGraphSize|BenchmarkOutDegreeByType$|BenchmarkPropRead|BenchmarkLabelRead|BenchmarkExistence|BenchmarkEdgeSideRead_LabelsByHandle$|BenchmarkTombstoneScanClean'
EXEC='.'

PKGS="cypher_exec cypher store_wal store_txn graph_lpg"

sel_for() {
  case "$1" in
    cypher_exec) printf '%s' "$EXEC" ;;
    cypher)      printf '%s' "$CY" ;;
    store_wal)   printf '%s' "$WAL" ;;
    store_txn)   printf '%s' "$TXN" ;;
    graph_lpg)   printf '%s' "$LPG" ;;
  esac
}

EXITLOG=$OUT/exits.log
LOADLOG=$OUT/loadavg.log
: > "$EXITLOG"
: > "$LOADLOG"

echo "START $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$EXITLOG"

for r in $(seq 1 $ROUNDS); do
  # Rotate the arm order every round: 1->A B B2, 2->B B2 A, 3->B2 A B, 4->A B B2 ...
  case $(( (r - 1) % 3 )) in
    0) ARMS="A B B2" ;;
    1) ARMS="B B2 A" ;;
    2) ARMS="B2 A B" ;;
  esac
  for pkg in $PKGS; do
    sel=$(sel_for "$pkg")
    for arm in $ARMS; do
      echo "round=$r pkg=$pkg arm=$arm BEFORE $(uptime)" >> "$LOADLOG"
      "$SP/bin/${pkg}_${arm}.test" \
          -test.run=XXXNONE \
          -test.bench="$sel" \
          -test.benchmem \
          -test.count=1 \
        >> "$OUT/${pkg}_${arm}.txt" \
        2>> "$OUT/${pkg}_${arm}.stderr"
      ec=$?
      echo "round=$r pkg=$pkg arm=$arm EXIT=$ec" >> "$EXITLOG"
      echo "round=$r pkg=$pkg arm=$arm AFTER  $(uptime)" >> "$LOADLOG"
    done
  done
  echo "ROUND $r COMPLETE $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$EXITLOG"
done

echo "END $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$EXITLOG"
echo "ALLDONE"
