# GoGraph examples

This directory contains 26 runnable, self-documenting demonstrations of
GoGraph — from a five-line shortest-path query to a persistent,
`kill -9`-safe REST service. Each example is a standalone `package main`
with its own `README.md`, and each is pinned by a regression test so the
output you see below is guaranteed by CI, not just illustrative.

Run any example with:

```bash
go run ./examples/<NN_name>
```

For the conventions every example follows — testable extraction, a
regression test, and the per-example `README.md` template — see
[`../docs/examples-standard.md`](../docs/examples-standard.md).

## Basics

| Example | What it demonstrates | Run |
|---|---|---|
| [01_basic](01_basic/README.md) | Build a weighted directed graph, freeze it into an immutable CSR snapshot, and run single-source `search.Dijkstra`, reading back both distances and reconstructed routes. | `go run ./examples/01_basic` |
| [02_property_graph](02_property_graph/README.md) | Build a labelled property graph with an optional schema, run label- and property-indexed `MATCH`-style queries, and read typed properties back out. | `go run ./examples/02_property_graph` |
| [03_advanced_algorithms](03_advanced_algorithms/README.md) | Run four algorithms over one CSR snapshot — BFS, Dijkstra, exact Brandes betweenness centrality (with printed ranks), and PageRank. | `go run ./examples/03_advanced_algorithms` |

## Persistence and out-of-core

| Example | What it demonstrates | Run |
|---|---|---|
| [04_persistence](04_persistence/README.md) | The full durability path: WAL-committed transactions, a v2 snapshot (CSR + labels + properties), then rebuild from disk with `recovery.Open`. | `go run ./examples/04_persistence` |
| [05_out_of_core](05_out_of_core/README.md) | Tier 2 external memory: persist a CSR snapshot as a `csrfile`, re-open it by `mmap`, and run semi-external PageRank over the mapped adjacency. | `go run ./examples/05_out_of_core` |
| [17_transactional_log](17_transactional_log/README.md) | WAL-backed store with a background checkpointer that folds the log into a self-sufficient snapshot, plus recovery after a simulated crash. | `go run ./examples/17_transactional_log` |
| [18_oocore_pipeline](18_oocore_pipeline/README.md) | The full out-of-core pipeline: CSV → CSR → `csrfile` → `mmap`, then semi-external BFS and PageRank over the mapped region. | `go run ./examples/18_oocore_pipeline` |
| [21_typed_recovery](21_typed_recovery/README.md) | Generic `recovery.Open[N, W]` over an `(int64, float64)` graph: round-trip edges (bit-exact float weights), labels, and typed properties through a v2 snapshot. | `go run ./examples/21_typed_recovery` |

## Cypher and Bolt

| Example | What it demonstrates | Run |
|---|---|---|
| [22_cypher](22_cypher/README.md) | The Cypher engine over a small social graph: a label scan with projection and `ORDER BY`, a `WHERE` filter, a relationship pattern, and a `CREATE` in a write transaction — every value printed in human-readable form. | `go run ./examples/22_cypher` |
| [23_bolt_server](23_bolt_server/README.md) | Bolt v5 end to end: start the embedded server, connect the official `neo4j-go-driver/v5` as a real client, run a Cypher query over a session, and shut down cleanly with no goroutine leak. | `go run ./examples/23_bolt_server` |
| [24_social_network_cli](24_social_network_cli/README.md) | A one-shot CLI over a persistent LPG social network, walking every layer: LPG, WAL + recovery, manual checkpoints, and Cypher reads streamed as JSON Lines. | `go run ./examples/24_social_network_cli` |
| [25_software_house_api](25_software_house_api/README.md) | A persistent, `kill -9`-safe REST API (stdlib only) over a multi-layer LPG spanning Code/Work/People, answering change-impact, ownership, and bus-factor questions in Cypher. | `go run ./examples/25_software_house_api` |

## Interchange

| Example | What it demonstrates | Run |
|---|---|---|
| [06_csv_import](06_csv_import/README.md) | The serialisation round-trip: read an edge-list CSV with `csv.ReadInto`, then write the graph back out as CSV and as newline-delimited JSON (JSON Lines). | `go run ./examples/06_csv_import` |
| [07_graphml_roundtrip](07_graphml_roundtrip/README.md) | Graph interchange I/O: parse a GraphML document with `graphml.ReadInto`, then serialise it back out to GraphML and Graphviz DOT, edges and weights intact. | `go run ./examples/07_graphml_roundtrip` |

## Algorithms

| Example | What it demonstrates | Run |
|---|---|---|
| [08_pagerank](08_pagerank/README.md) | PageRank over a directed authority web (peripheral pages → an authority → a hub), reading back the per-node rank vector ordered most- to least-important with distinct ranks. | `go run ./examples/08_pagerank` |
| [09_leiden](09_leiden/README.md) | Modularity-optimising community detection with `community.Leiden` over two K4 cliques joined by a single bridge edge. | `go run ./examples/09_leiden` |
| [10_dimacs9_routing](10_dimacs9_routing/README.md) | Build a deterministic synthetic road network with the DIMACS 9 harness, then run a concrete `search.Dijkstra` query that reconstructs the shortest route from node 0 to node 11. | `go run ./examples/10_dimacs9_routing` |
| [14_routing_alternatives](14_routing_alternatives/README.md) | Three shortest-path flavours over one routing graph: Dijkstra, Yen's k-shortest paths, and `search.AStar` driven by a coordinate-based Euclidean heuristic that expands fewer nodes for the same optimal cost. | `go run ./examples/14_routing_alternatives` |
| [15_task_assignment](15_task_assignment/README.md) | Two bipartite assignment algorithms side by side: `search.Hungarian` (cheapest one-to-one assignment) and `search.HopcroftKarp` (largest matching once edges are pruned). | `go run ./examples/15_task_assignment` |
| [16_centrality_analytics](16_centrality_analytics/README.md) | Two analytics over one CSR snapshot: exact Brandes betweenness centrality and label-propagation community detection, with deterministic tie-breaking. | `go run ./examples/16_centrality_analytics` |

## Real-world recipes

| Example | What it demonstrates | Run |
|---|---|---|
| [11_social_network](11_social_network/README.md) | An end-to-end social-network workload over an LPG: PageRank influence ranking, Leiden community detection, and a manual friend-of-friend recommendation walk. | `go run ./examples/11_social_network` |
| [12_build_dependency](12_build_dependency/README.md) | Model a build-dependency graph, derive a valid build order with `search.TopologicalSort` (Kahn), and detect a circular dependency with `search.TarjanSCC`. | `go run ./examples/12_build_dependency` |
| [13_network_reliability](13_network_reliability/README.md) | Two resilience analyses over one network: single points of failure (articulation points and bridges) and the max throughput plus its limiting min-cut bottleneck, with the flow network derived from the same edge list. | `go run ./examples/13_network_reliability` |
| [19_pattern_query](19_pattern_query/README.md) | The fluent `graph/query` API: MATCH-style pattern queries combining label and property predicates with a one-hop expansion, reading matched properties back out. | `go run ./examples/19_pattern_query` |
| [20_concurrent_reads](20_concurrent_reads/README.md) | The lock-free read contract of a frozen CSR: Dijkstra, BFS, and PageRank run concurrently over one shared immutable snapshot with zero synchronisation on the snapshot. | `go run ./examples/20_concurrent_reads` |

## Benchmarks

| Example | What it demonstrates | Run |
|---|---|---|
| [26_social_scale_bench](26_social_scale_bench/README.md) | A large-scale social network (up to 1M users, 30k articles, `FRIEND` and `LIKE` edges) built in memory and queried with Cypher, reporting build throughput, Go heap footprint, and per-query latency — a benchmark for query performance and resource consumption. Scale-parametrised via flags. | `go run ./examples/26_social_scale_bench` |
