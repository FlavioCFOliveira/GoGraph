# blocks.sh — the block table, sourced by both the sizing run and the campaign.
# Fields separated by '#'. NOT by '|': the selectors are regexes whose alternation
# uses '|', so a '|' delimiter splits the selector itself. That defect was caught by
# the sizing run (block=cy EXIT=1 RESULTLINES=0) and is why every block logs its own
# exit code and its own result-line count.
# Fields: label # binary-stem # selector # cpu-ladder(y/n)
CY_SEL='^(BenchmarkParallelScan_|BenchmarkParallelScanProject_|BenchmarkParallelLabelScan_|BenchmarkParallelAggregate_|BenchmarkParallelScanGate|BenchmarkPlanCacheHit_|BenchmarkPlanCacheMiss_|BenchmarkCount|BenchmarkReadOnly_|BenchmarkCreateRelationships|BenchmarkDeleteAccumulated|BenchmarkMergeMatch_|BenchmarkExpandInto_|BenchmarkColumnarShape_Hop|BenchmarkScalarFilterProjection|BenchmarkZZReorder)'
SEARCH_SEL='^(BenchmarkDijkstra_PostWarmup|BenchmarkDijkstra_Large|BenchmarkBFSDirectionOpt_PowerLaw|BenchmarkYen_K100)$'

BLOCKS=(
  "exec#cypher_exec#^Benchmark#n"
  "cy#cypher#$CY_SEL#n"
  "wal#store_wal#^Benchmark#n"
  "txn#store_txn#^BenchmarkCommit\$#n"
  "search#search#$SEARCH_SEL#n"
  "centrality#search_centrality#^BenchmarkBrandes_RandomGraph\$#n"
  "Lreadtx#cypher#^BenchmarkReadTx_(LockFree|WriterLock)\$#y"
  "Lcypar#cypher#^(BenchmarkPlanReusePhasesParallel|BenchmarkAdjacencyCacheLookup|BenchmarkPrebuiltTreeCeilingParallel)\$#y"
  "Llpg#graph_lpg#^(BenchmarkNodeMetadataReadParallel|BenchmarkBarrier_BareRWMutexParallel)\$#y"
  "Lmvcc#graph_mvcc#^(BenchmarkGate_WeakParallel|BenchmarkPublishConcurrent|BenchmarkRWMutexShared_Parallel)\$#y"
  "Lprom#internal_metrics_prometheus#^BenchmarkIncCounterParallel\$#y"
  "Lmapper#graph#^BenchmarkMapper_Intern_HotKey_Parallel\$#y"
  "Ltxn#store_txn#^BenchmarkCommitConcurrent\$#n"
  "Lbolt#bolt_server#^BenchmarkBoltRecordEncodeParallel\$#y"
)
