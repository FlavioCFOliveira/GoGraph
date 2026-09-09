# blocks.sh — the block table for the v0.14.1 campaign, sourced by the sizing run
# and by the campaign itself.
#
# Fields are separated by '#', NOT by '|': the selectors are regexes whose
# alternation is also '|', so a '|' delimiter splits the selector itself. That
# defect was caught by the v0.14.0 sizing run (block=cy EXIT=1 RESULTLINES=0),
# which is why every block still logs its own exit code and result-line count.
#
# Fields: label # binary-stem # selector # cpu-ladder(y/n) # arms(all|BB2)
#
# arms=BB2 marks a block whose benchmarks do not EXIST at v0.14.0, so there is no
# A arm to compare against. Such a block still yields the within-arm floor and an
# absolute figure, and is reported as one-armed rather than silently compared.
#
# ============================================================================
# WHY THIS SET, AND WHY IT IS SMALLER THAN v0.14.0's
# ============================================================================
# v0.14.1 is a PATCH release of 50 commits that are overwhelmingly bug fixes.
# A campaign proportionate to it measures two things and says plainly what it
# left alone:
#
#   (a) the HEADLINE set, so the release stays comparable with its predecessors
#       -- exactly scripts/run_headline_bench.sh's five benchmarks; and
#   (b) the surfaces this window genuinely touched -- cypher/, cypher/exec,
#       store/recovery, store/txn, store/checkpoint and bolt/server;
#
# plus the one byte-identical package that makes ADJUDICATION possible (the
# cross-arm floor), and the two concurrency ladders the README publishes, so
# those tables are RE-MEASURED rather than relabelled.
#
# Measured per-block costs (arm B, one invocation) are in sizing.log. The set
# below was cut from 23 blocks to 11 on those costs. What was dropped, and why:
#
#   cy (full 149-benchmark package)  454 s -> trimmed to the changed surface.
#       The full package costs more wall-clock than its evidentiary value on a
#       patch release: 3 arms x 6 rounds x 454 s is 2.3 hours for one block.
#   idx, btree, hash   graph/index{,/btree,/hash} changed, but their benchmarks
#       price data-structure primitives that no fix in this window touched;
#       the index DEFECTS were correctness, and the guards' cost is measured at
#       task level (#2778, #2792, #2793) and in the exec/cy blocks.
#   lpg                graph/lpg changed (readview.go, mvcc_index.go). Its 33
#       benchmarks include the ReadScale1671_* family that v0.14.0 also skipped
#       on cost. #2775's own +3.58 % figure is quoted at task level instead.
#   snap, wal          store/snapshot and store/wal changed additively
#       (index_builder_epoch; a godoc). Dropped on evidentiary value.
#   Lcypar, Llpg, Lbolt, Lmvcc, Lprom, Lmapper   six further ladders, each five
#       -test.cpu levels. The README publishes two ladders, and those two are
#       kept; these six would have cost more than they proved here.
#
# Every one of those is listed in docs/benchmarks/v0.14.1.md under
# "what this snapshot does NOT measure", with this reason.

SEARCH_SEL='^(BenchmarkDijkstra_PostWarmup|BenchmarkDijkstra_Large|BenchmarkBFSDirectionOpt_PowerLaw|BenchmarkYen_K100)$'

# CY_SEL — the cypher benchmarks that price THIS window's changed surface, plus
# two control families whose source did not change. Grouped by the task that
# makes each relevant, so a reader can check the selection against the diff:
#   #2814/#2778/#2792/#2793/#2799/#2798  the index access path
#   #2629                                the far-endpoint label admission
#   #2740                                the plan-cache single flight
#   #2662                                the ORDER BY key hoist
#   #2660/#2675/#2779/#2781/#2747        the write path
#   #2772/#2785                          the planner statistics
#   #2777                                the count-store leaves
#   controls                             RowCtxPool, ParallelScan_Count
CY_SEL='^(BenchmarkIndexSeek_vs_LabelScan|BenchmarkIndexedPointLookup|BenchmarkIndexedNumericRange|BenchmarkNumericEqualitySeek_|BenchmarkNumericRangeSeek_|BenchmarkIndexIntersectPlan_|BenchmarkRecoveredIndexPopulation|BenchmarkColumnarShape_Hop|BenchmarkPlanCacheHit_|BenchmarkPlanCacheMiss_|BenchmarkPlanReusePhases$|BenchmarkPrebuiltTreeCeiling$|BenchmarkResultMaterialize_|BenchmarkScalarFilterProjection|BenchmarkCreateRelationships|BenchmarkDeleteAccumulated|BenchmarkMergeUnwindBatch|BenchmarkBlockFormCount|BenchmarkQueryHasWritingClause|BenchmarkJoinReorderStats|BenchmarkReorderDrainCost|BenchmarkCountAllNodes|BenchmarkCountNodeVar|BenchmarkCountPushdown_|BenchmarkCount_Label|BenchmarkRowCtxPool_|BenchmarkParallelScan_Count)'

BLOCKS=(
  # --- (b) the changed surface ---------------------------------------------
  "exec#cypher_exec#^Benchmark#n#all"
  "cy#cypher#$CY_SEL#n#all"
  "rec#store_recovery#^Benchmark#n#all"
  "txn#store_txn#^BenchmarkCommit\$#n#all"
  "bolt#bolt_server#^Benchmark#n#all"
  # store/checkpoint has ZERO benchmarks at v0.14.0 -- all five are new in this
  # window and price the #2749/#2780 readback. One-armed by construction.
  "ckpt#store_checkpoint#^Benchmark#n#BB2"
  # --- (a) the headline set, and the controls it doubles as -----------------
  # No file under search/ changed (git diff --name-only v0.14.0..HEAD -- search/
  # is empty), so a large delta here is evidence the MEASUREMENT is wrong.
  "search#search#$SEARCH_SEL#n#all"
  "centrality#search_centrality#^BenchmarkBrandes_RandomGraph\$#n#all"
  # --- the cross-arm FLOOR: byte-identical test binary between the releases --
  "cnt#graph_index_count#^Benchmark#n#all"
  # --- the two ladders the README publishes, 1/8/64/256/1024 ---------------
  "Lreadtx#cypher#^BenchmarkReadTx_(LockFree|WriterLock)\$#y#all"
  "Ltxn#store_txn#^BenchmarkCommitConcurrent\$#n#all"
)
