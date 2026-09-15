# blocks.sh — the block table for the v0.14.2 campaign, sourced by the sizing run
# and by the campaign itself.
#
# Fields are separated by '#', NOT by '|': the selectors are regexes whose
# alternation is also '|', so a '|' delimiter splits the selector itself. That
# defect was caught by the v0.14.0 sizing run (block=cy EXIT=1 RESULTLINES=0),
# which is why every block still logs its own exit code and result-line count.
#
# Fields: label # binary-stem # selector # cpu-ladder(y/n) # arms(all|BB2)
#
# ============================================================================
# WHY THIS SET
# ============================================================================
# v0.14.2 is a bug-fixing sprint. The engine delta is FIVE non-test files in
# THREE packages:
#
#   cypher/api.go        dropIndexCounter / runDropIndex (the DROP INDEX
#                        existence read), isEntityPropertyValue /
#                        errEntityPropertyValue and one new scalarColSnapshot
#                        call on the SET value evaluator, Result.counters
#                        dropped on a rolled-back statement, godoc.
#   cypher/exec/set.go   relStorageDirection: the relationship SET write target
#                        is normalised from traversal order to storage order.
#   cypher/exec/set_all.go   the REPLACE-form arm of the same rejection.
#   bolt/server/errors.go    three new Neo.ClientError.* branches in FailureCode.
#   bolt/server/serve.go     godoc only — no compiled change.
#
# Everything else in the window is *_test.go, internal/sim (the DST simulator's
# oracle) or documentation. Every other package in the module is byte-for-byte
# the v0.14.1 tree.
#
# The set below is therefore three things and nothing else:
#
#  (a) harness — PURPOSE-BUILT, because the module's own corpus does not drive
#      the changed write paths. Scanned from source: of 149 `cypher`
#      benchmarks exactly one names a SET clause (BenchmarkQueryHasWritingClause,
#      which classifies a query string without executing it), of 36 `cypher/exec`
#      benchmarks none does, and no benchmark anywhere issues DROP INDEX. Two
#      cypher benchmarks (ReadPhaseForeignWriter, ReadPhaseAttribution) run a
#      throttled background `SET n.bal = $v` while MEASURING a read; they price
#      the read, not the write, and they are excluded for that reason.
#
#  (b) exec / cy / bolt — the three packages whose TEST BINARY changed, run as a
#      whole-package regression watch. No benchmark in them reaches the changed
#      symbols, so what these blocks actually test is whether the edit moved
#      anything through code motion, inlining or layout. That is a real risk and
#      it is exactly what a reader worries about when a hot package changes.
#
#  (c) search / centrality / cnt — the release gate's headline set and the
#      v0.14.1 cross-arm floor. All three compile to a test binary that is
#      BYTE-IDENTICAL between v0.14.1 and v0.14.2 (sha256, binary-sha256.txt),
#      so every A-vs-B row they produce is noise BY CONSTRUCTION, measured across
#      the arms. They are the adjudication equipment, and the headline numbers
#      are re-measured rather than relabelled.

SEARCH_SEL='^(BenchmarkDijkstra_PostWarmup|BenchmarkDijkstra_Large|BenchmarkBFSDirectionOpt_PowerLaw|BenchmarkYen_K100)$'

# CY_SEL — carried over VERBATIM from the v0.14.1 campaign so the two records
# compare like with like. It is 60 of the package's 149 benchmarks, chosen there
# to price the index access path, the plan cache, result materialisation, the
# write path (CreateRelationships, DeleteAccumulated, MergeUnwindBatch — the
# three that reach the property-evaluator builders this window edited), the
# planner statistics, the count-store leaves, and two control families.
CY_SEL='^(BenchmarkIndexSeek_vs_LabelScan|BenchmarkIndexedPointLookup|BenchmarkIndexedNumericRange|BenchmarkNumericEqualitySeek_|BenchmarkNumericRangeSeek_|BenchmarkIndexIntersectPlan_|BenchmarkRecoveredIndexPopulation|BenchmarkColumnarShape_Hop|BenchmarkPlanCacheHit_|BenchmarkPlanCacheMiss_|BenchmarkPlanReusePhases$|BenchmarkPrebuiltTreeCeiling$|BenchmarkResultMaterialize_|BenchmarkScalarFilterProjection|BenchmarkCreateRelationships|BenchmarkDeleteAccumulated|BenchmarkMergeUnwindBatch|BenchmarkBlockFormCount|BenchmarkQueryHasWritingClause|BenchmarkJoinReorderStats|BenchmarkReorderDrainCost|BenchmarkCountAllNodes|BenchmarkCountNodeVar|BenchmarkCountPushdown_|BenchmarkCount_Label|BenchmarkRowCtxPool_|BenchmarkParallelScan_Count)'

BLOCKS=(
  # --- (a) the changed write paths, on a purpose-built vehicle --------------
  "harness#benchharness#^Benchmark#n#all"
  # --- (b) the three packages whose binary changed -------------------------
  "exec#cypher_exec#^Benchmark#n#all"
  "cy#cypher#$CY_SEL#n#all"
  "bolt#bolt_server#^Benchmark#n#all"
  # --- (c) headline set AND cross-arm floor (byte-identical binaries) ------
  "search#search#$SEARCH_SEL#n#all"
  "centrality#search_centrality#^BenchmarkBrandes_RandomGraph\$#n#all"
  "cnt#graph_index_count#^Benchmark#n#all"
)
