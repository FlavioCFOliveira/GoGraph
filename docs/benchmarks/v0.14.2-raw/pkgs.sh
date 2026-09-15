# pkgs.sh — the packages compiled for the v0.14.2 campaign, and the stem each
# binary is named with. Sourced by build.sh and blocks.sh.
#
# Seven packages, not eighteen. The v0.14.2 window changes FIVE non-test engine
# files in THREE packages (cypher, cypher/exec, bolt/server); the rest of the
# module is byte-for-byte the v0.14.1 tree. The set below is therefore:
#   * the three packages whose source changed            -> effects / regression watch
#   * the two headline packages the release gate runs    -> continuity + cross-arm floor
#   * graph/index/count, the v0.14.1 cross-arm floor     -> floor continuity
#   * benchharness, purpose-built for this release       -> the changed write paths
#
# Fields: import-path # binary-stem
PKGS=(
  "cypher#cypher"
  "cypher/exec#cypher_exec"
  "bolt/server#bolt_server"
  "search#search"
  "search/centrality#search_centrality"
  "graph/index/count#graph_index_count"
  "benchharness#benchharness"
)
