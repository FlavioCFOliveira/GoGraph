# pkgs.sh — the packages compiled for the v0.14.1 campaign, and the stem each
# binary is named with. Sourced by build.sh and blocks.sh.
# Fields: import-path # binary-stem
PKGS=(
  "cypher#cypher"
  "cypher/exec#cypher_exec"
  "graph#graph"
  "graph/index#graph_index"
  "graph/index/btree#graph_index_btree"
  "graph/index/hash#graph_index_hash"
  "graph/index/count#graph_index_count"
  "graph/lpg#graph_lpg"
  "graph/mvcc#graph_mvcc"
  "store/txn#store_txn"
  "store/checkpoint#store_checkpoint"
  "store/recovery#store_recovery"
  "store/snapshot#store_snapshot"
  "store/wal#store_wal"
  "bolt/server#bolt_server"
  "search#search"
  "search/centrality#search_centrality"
  "internal/metrics/prometheus#internal_metrics_prometheus"
)
