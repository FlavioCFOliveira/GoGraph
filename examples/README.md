# GoGraph examples

This directory contains 26 runnable, self-documenting examples of
GoGraph — from a shortest-path query to a persistent, `kill -9`-safe REST
service. Each example serves the two objectives the project sets for
examples: it **demonstrates** a capability in a realistic end-to-end
application, and it **exercises** the module through a realistic, seeded,
scale-parametrised scenario while **collecting evidence** — timing and
throughput, memory and allocation, contention, and correctness. Each is a
standalone `package main` with its own `README.md`, and each pins its
deterministic facts with a regression test so the results below are
guaranteed by CI, not just illustrative.

Run any example at its small deterministic default, or scale it up:

```bash
go run ./examples/<NN_name>                 # small deterministic default
go run ./examples/<NN_name> -h              # the scale/shape flags it accepts
```

Output is split into deterministic **facts** (bare lines, pinned by the
test) and volatile **telemetry** (lines prefixed with `# ` — durations,
throughput, heap — which vary per run and per machine).

For the full contract every example follows — the realistic seeded
generator, scale knobs, the evidence taxonomy, the `# ` telemetry
convention, testable extraction, and the per-example `README.md` template —
see [`../docs/examples-standard.md`](../docs/examples-standard.md).
`examples/26_social_scale_bench` is the reference end state.

## Basics

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [01_basic](01_basic/README.md) | Build a seeded weighted directed road network, freeze it into an immutable CSR snapshot, and run single-source `search.Dijkstra`, reading back distances and reconstructed routes. | Build throughput, Dijkstra query latency, reachable-node count, live heap. |
| [02_property_graph](02_property_graph/README.md) | Build a seeded labelled property graph with an optional schema validator, run label- and property-indexed `MATCH`-style queries, and read typed properties back out. | Build throughput, indexed-query latency, live heap and bytes per node. |
| [03_advanced_algorithms](03_advanced_algorithms/README.md) | Run four algorithms over one CSR snapshot of a seeded scale-free graph — BFS, Dijkstra, exact Brandes betweenness centrality, and PageRank. | Per-algorithm timing, PageRank iterations, transient allocations, live heap. |

## Persistence and out-of-core

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [04_persistence](04_persistence/README.md) | The full durability path over a seeded graph: WAL-committed transactions, a v2 snapshot (CSR + labels + properties), then rebuild from disk with `recovery.Open`. | Commit throughput, WAL/snapshot bytes on disk, recovery time, heap before/after. |
| [05_out_of_core](05_out_of_core/README.md) | Tier 2 external memory: persist a seeded CSR snapshot as a `csrfile`, re-open it by `mmap`, and run semi-external PageRank over the mapped adjacency. | On-disk size vs resident heap (the out-of-core advantage), mmap and query time. |
| [17_transactional_log](17_transactional_log/README.md) | WAL-backed store over a seeded financial ledger with a background checkpointer that folds the log into a self-sufficient snapshot, plus recovery after a simulated crash. | Write throughput, WAL bytes folded, snapshot bytes, checkpoint count, recovery time. |
| [18_oocore_pipeline](18_oocore_pipeline/README.md) | The full out-of-core pipeline over a seeded graph: CSV → CSR → `csrfile` → `mmap`, then semi-external BFS and PageRank over the mapped region. | Per-stage timing (parse/build/write/mmap/BFS/PageRank), on-disk size vs heap. |
| [21_typed_recovery](21_typed_recovery/README.md) | Generic `recovery.Open[N, W]` over a seeded `(int64, float64)` graph: round-trip edges (bit-exact float weights), labels, and typed properties through a v2 snapshot. | Snapshot bytes, recovery time, heap, and a bit-exact float64 round-trip verification. |

## Cypher and Bolt

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [22_cypher](22_cypher/README.md) | The Cypher engine over a seeded social graph: a label scan with projection and `ORDER BY`, a `WHERE` filter, a relationship pattern, and a `CREATE` in a write transaction. | Per-query latency, live heap. |
| [23_bolt_server](23_bolt_server/README.md) | Bolt v5 end to end over a seeded graph: start the embedded server, connect the official `neo4j-go-driver/v5`, run many queries across concurrent sessions, and shut down cleanly with no goroutine leak. | Query throughput, p50/p95/p99 latency distribution, live heap. |
| [24_social_network_cli](24_social_network_cli/README.md) | A one-shot CLI over a persistent LPG social network with an opt-in seeded scale mode, walking every layer: LPG, WAL + recovery, manual checkpoints, and Cypher reads streamed as JSON Lines. | Seed throughput, live heap, per-query latency (via the `-evidence` flag). |
| [25_software_house_api](25_software_house_api/README.md) | A persistent, `kill -9`-safe REST API (stdlib only) over a multi-layer LPG spanning Code/Work/People with an opt-in seeded scale mode, answering change-impact, ownership, and bus-factor questions in Cypher. | Graph size, live heap, bytes per element, per-query/seed latency (via `/stats`). |

## Interchange

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [06_csv_import](06_csv_import/README.md) | The serialisation round-trip over a seeded edge list: read it with `csv.ReadInto`, then write the graph back out as CSV and as newline-delimited JSON (JSON Lines). | Parse and serialise throughput (rows/s, MiB/s), bytes in/out, live heap. |
| [07_graphml_roundtrip](07_graphml_roundtrip/README.md) | Graph interchange I/O over a seeded graph: parse a GraphML document with `graphml.ReadInto`, then serialise it back out to GraphML and Graphviz DOT, edges and weights intact. | Parse and serialise throughput, bytes in/out per format, live heap. |

## Algorithms

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [08_pagerank](08_pagerank/README.md) | PageRank over a seeded directed scale-free web (heavy-tailed in-degree), reading back the per-node rank vector ordered most- to least-important. | Convergence iterations, timing, transient allocations, live heap. |
| [09_leiden](09_leiden/README.md) | Modularity-optimising community detection with `community.Leiden` over a seeded stochastic-block-model graph of planted communities. | Detection timing, achieved modularity, communities found. |
| [10_dimacs9_routing](10_dimacs9_routing/README.md) | Build a synthetic DIMACS 9 road network at scale, run a concrete `search.Dijkstra` route, and drive a seeded random source-target probe workload. | Query throughput, p50/p95/p99 latency distribution, live heap. |
| [14_routing_alternatives](14_routing_alternatives/README.md) | Three shortest-path flavours over one seeded k-NN spatial graph: Dijkstra, Yen's k-shortest paths, and `search.AStar` with an admissible Euclidean heuristic. | Per-algorithm timing, nodes expanded (the A* vs Dijkstra advantage), live heap. |
| [15_task_assignment](15_task_assignment/README.md) | Two bipartite assignment algorithms over a seeded instance: `search.Hungarian` (cheapest one-to-one assignment) and `search.HopcroftKarp` (largest matching). | Per-algorithm timing, live heap. |
| [16_centrality_analytics](16_centrality_analytics/README.md) | Two analytics over one CSR snapshot of a seeded chain-of-clusters graph: exact Brandes betweenness centrality and label-propagation community detection. | Per-analysis timing, transient allocations, live heap. |

## Real-world recipes

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [11_social_network](11_social_network/README.md) | An end-to-end social-network workload over a seeded LPG: PageRank influence ranking, Leiden community detection, and a manual friend-of-friend recommendation walk. | Per-stage timing (PageRank/Leiden/FoF), live heap. |
| [12_build_dependency](12_build_dependency/README.md) | Model a seeded build-dependency DAG, derive a valid build order with `search.TopologicalSort` (Kahn), and detect a circular dependency with `search.TarjanSCC`. | Per-algorithm timing, DAG statistics, live heap. |
| [13_network_reliability](13_network_reliability/README.md) | Two resilience analyses over one seeded transit-stub network: single points of failure (articulation points and bridges) and max throughput plus its limiting min-cut bottleneck. | Per-analysis timing, max-flow value, min-cut size, live heap. |
| [19_pattern_query](19_pattern_query/README.md) | The fluent `graph/query` API over a seeded dependency LPG: MATCH-style pattern queries combining label and property predicates with a one-hop expansion, reading matched properties back out. | Per-query latency, matched-row counts, live heap. |
| [20_concurrent_reads](20_concurrent_reads/README.md) | The lock-free read contract of a frozen CSR: Dijkstra, BFS, and PageRank run concurrently over one shared immutable seeded snapshot with zero synchronisation on the snapshot. | Aggregate throughput, scaling across worker counts, live heap. |

## Benchmarks

| Example | What it demonstrates | Evidence reported |
|---|---|---|
| [26_social_scale_bench](26_social_scale_bench/README.md) | A large-scale social network (up to 1M users, 30k articles, `FRIEND` and `LIKE` edges) built in memory and queried with Cypher — the reference end state for this standard, scale-parametrised via flags. | Build throughput, Go heap footprint, bytes per edge, per-query latency. |
