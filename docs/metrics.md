# Metrics Inventory

This document enumerates every observability metric exported by
GoGraph's public blocking APIs. It is the authoritative companion
to the `internal/metrics` package and the CLAUDE.md mandate on
"latency histograms on every public blocking API".

The metrics are emitted through the `internal/metrics.Backend`
interface. The default backend is a no-op; the cost of an
unconfigured metric site is two atomic loads and one `time.Now()`
pair (~50ns per call site). Installing a `Backend` via
`metrics.SetBackend` activates dispatch. A Prometheus-compatible
backend lives outside the dependency graph: any consumer that
implements the `Backend` interface can wire `gograph` into its own
`prometheus.Registry` without forcing `prometheus/client_golang`
into the module graph.

## Naming convention

Every metric name follows the schema

    <package-path>.<ExportedSymbol>[.errors]

where:

* `<package-path>` is the dotted module path, all lower-case
  (`search`, `search.centrality`, `search.flow`, `store.wal`,
  `graph.io.csv`, ...). It uses dots — never slashes — so the names
  remain valid as Prometheus / OpenMetrics labels.
* `<ExportedSymbol>` is the exact name of the public Go function or
  method that the metric instruments, preserving its original case
  (`Dijkstra`, `BFSCtx`, `WriteCtx`, `AppendCtx`, ...).
* The optional `.errors` suffix indicates a counter that counts
  occurrences of error-returning paths in that symbol. Latency
  observations (the bare `<package>.<Symbol>` form) double as call
  counters when the backend exposes histogram counts; no separate
  `.calls` counter is exported.

For paired `Foo` / `FooCtx` entry points (where `Foo` delegates to
`FooCtx`), both names fire once per call. This is intentional: the
top-level entry that the caller used is the one that appears in
their request span, and it carries any added overhead unique to
that wrapper. The two latency series remain comparable because the
underlying work is the same.

## Latency overhead

Every wired call site pays a constant per-call cost determined by
the active backend:

* **No-op backend (default)**: two atomic-pointer loads + one
  `time.Now` pair, ~50ns on Apple M4 — the
  `bench/comparison.BenchmarkComparison_Dijkstra` headline benchmark
  shows <1% regression versus the pre-wire baseline.
* **Counting backend (testing, see `internal/metrics/wireup_test.go`)**:
  one mutex acquire + one map insertion or counter add per event.
  Used by the smoke test to assert metric names; not intended for
  production use.
* **Prometheus backend (out-of-tree)**: cost is bounded by the
  Prometheus histogram implementation the consumer wires in.

The instrumentation is intentionally confined to the public entry
point of each blocking operation. Inner hot loops (heap-pop, BFS
neighbour scan, CSR edge walk) are NOT instrumented: a per-edge or
per-pop call to `metrics.Time` would dominate the algorithm's own
work and is explicitly forbidden by the wire-up guidelines.

## Metric inventory

The complete list of wired latency observations is grouped below by
package. Every entry has, in addition, a paired `.errors` counter
that increments once per failing path (a returned non-nil `error`
or a cancellation-driven return). The `.errors` counter is omitted
for functions whose only outcome is non-error (e.g. `search.BFS`
with no return value).

### `search`

| Metric                                     | Description                                                          |
| ------------------------------------------ | -------------------------------------------------------------------- |
| `search.AStar`                             | A* shortest-path entry without context.                              |
| `search.AStarCtx`                          | A* shortest-path entry with context.                                 |
| `search.AStarInto`                         | Zero-allocation A* primitive into caller-provided scratch.           |
| `search.BellmanFord`                       | Bellman-Ford single-source shortest paths.                           |
| `search.BellmanFordCtx`                    | Bellman-Ford with context (SPFA inside).                             |
| `search.BellmanFordInto`                   | Zero-allocation Bellman-Ford primitive.                              |
| `search.BFS`                               | Breadth-first traversal entry without context.                       |
| `search.BFSCtx`                            | Breadth-first traversal with context.                                |
| `search.BFSDirectionOpt`                   | Direction-optimising BFS (Beamer 2012).                              |
| `search.BiBFS`                             | Bidirectional BFS shortest-path entry.                               |
| `search.BiBFSCtx`                          | Bidirectional BFS with context.                                      |
| `search.BiBFSOn`                           | BiBFS with caller-provided reverse CSR.                              |
| `search.BiBFSOnCtx`                        | BiBFS with reverse CSR and context.                                  |
| `search.BidirectionalDijkstra`             | Bidirectional Dijkstra point-to-point query.                         |
| `search.BidirectionalDijkstraOn`           | Bidirectional Dijkstra with caller-provided reverse CSR.             |
| `search.BidirectionalDijkstraOnCtx`        | Bidirectional Dijkstra core with reverse CSR and context.            |
| `search.CountTriangles`                    | Total and per-node triangle count.                                   |
| `search.CountTrianglesCtx`                 | Triangle count with context.                                         |
| `search.DFS`                               | Depth-first traversal entry without context.                         |
| `search.DFSCtx`                            | Depth-first traversal with context.                                  |
| `search.Diameter`                          | 2-sweep + iFUB diameter estimate.                                    |
| `search.DiameterCtx`                       | Diameter estimate with context.                                      |
| `search.Dijkstra`                          | Single-source Dijkstra entry without context.                        |
| `search.DijkstraCtx`                       | Single-source Dijkstra with context.                                 |
| `search.DijkstraInto`                      | Zero-allocation Dijkstra primitive.                                  |
| `search.DijkstraAPSP`                      | All-pairs Dijkstra (V * Dijkstra).                                   |
| `search.DijkstraAPSPCtx`                   | All-pairs Dijkstra with context.                                     |
| `search.JohnsonAPSP`                       | Johnson APSP (Bellman-Ford reweight + per-source Dijkstra).          |
| `search.JohnsonAPSPCtx`                    | Johnson APSP with context.                                           |
| `search.KShortestPathsLoopless`            | Best-first loopless k-shortest paths.                                |
| `search.KShortestPathsLooplessCtx`         | Best-first loopless k-shortest paths with context.                   |
| `search.EppsteinKShortest`                 | Deprecated alias for `search.KShortestPathsLoopless`.                |
| `search.EppsteinKShortestCtx`              | Deprecated alias for `search.KShortestPathsLooplessCtx`.             |
| `search.FloydWarshall`                     | All-pairs shortest paths (O(V^3)).                                   |
| `search.FloydWarshallCtx`                  | Floyd-Warshall with context.                                         |
| `search.Hierholzer`                        | Directed Eulerian circuit/path.                                      |
| `search.HierholzerCtx`                     | Directed Eulerian circuit/path with context.                         |
| `search.HierholzerUndirected`              | Undirected Eulerian circuit/path.                                    |
| `search.HierholzerUndirectedCtx`           | Undirected Eulerian circuit/path with context.                       |
| `search.HopcroftKarp`                      | Bipartite maximum-cardinality matching.                              |
| `search.HopcroftKarpCtx`                   | Bipartite matching with context.                                     |
| `search.HopcroftTarjanBCC`                 | Biconnected components + bridges + articulation points.              |
| `search.HopcroftTarjanBCCCtx`              | Hopcroft-Tarjan BCC with context.                                    |
| `search.Hungarian`                         | Rectangular assignment (Jonker-Volgenant).                           |
| `search.HungarianCtx`                      | Hungarian assignment with context.                                   |
| `search.KCore`                             | Coreness number per vertex.                                          |
| `search.KCoreCtx`                          | K-core decomposition with context.                                   |
| `search.KruskalMST`                        | Kruskal minimum spanning tree.                                       |
| `search.KruskalMSTCtx`                     | Kruskal MST with context.                                            |
| `search.PrimMST`                           | Prim minimum spanning tree.                                          |
| `search.PrimMSTCtx`                        | Prim MST with context.                                               |
| `search.TarjanSCC`                         | Tarjan strongly connected components.                                |
| `search.TarjanSCCCtx`                      | Tarjan SCC with context.                                             |
| `search.TopologicalSort`                   | Kahn topological sort.                                               |
| `search.TopologicalSortCtx`                | Topological sort with context.                                       |
| `search.TransitiveClosure`                 | Warshall bitset transitive closure.                                  |
| `search.TransitiveClosureCtx`              | Transitive closure with context.                                     |
| `search.WCC`                               | Weakly connected components (Union-Find).                            |
| `search.WCCCtx`                            | WCC with context.                                                    |
| `search.YenKShortest`                      | Yen k-shortest loopless paths.                                       |
| `search.YenKShortestCtx`                   | Yen k-shortest paths with context.                                   |

### `search/centrality`

| Metric                                                | Description                                                 |
| ----------------------------------------------------- | ----------------------------------------------------------- |
| `search.centrality.Betweenness`                       | Brandes betweenness centrality (unweighted).                |
| `search.centrality.BetweennessCtx`                    | Brandes betweenness with context.                           |
| `search.centrality.BetweennessParallel`               | Brandes betweenness parallelised across sources.            |
| `search.centrality.BetweennessParallelCtx`            | Parallel Brandes with context.                              |
| `search.centrality.WeightedBetweenness`               | Brandes betweenness with weighted shortest paths.           |
| `search.centrality.WeightedBetweennessCtx`            | Weighted Brandes with context.                              |
| `search.centrality.PageRank`                          | PageRank power iteration.                                   |
| `search.centrality.PageRankCtx`                       | PageRank with context.                                      |
| `search.centrality.PersonalisedPushPageRank`          | Andersen-Chung-Lang push PPR.                               |
| `search.centrality.PersonalisedPushPageRankCtx`       | Push PPR with context.                                      |

### `search/community`

| Metric                                       | Description                                            |
| -------------------------------------------- | ------------------------------------------------------ |
| `search.community.LabelPropagation`          | Raghavan-Albert-Kumara label propagation.              |
| `search.community.LabelPropagationCtx`       | Label propagation with context.                        |
| `search.community.Leiden`                    | Traag-Waltman-van Eck Leiden community detection.      |
| `search.community.LeidenCtx`                 | Leiden with context.                                   |

### `search/extern`

| Metric                              | Description                                                                  |
| ----------------------------------- | ---------------------------------------------------------------------------- |
| `search.extern.BFS`                 | Semi-external BFS over a Tier 2 csrfile.Reader.                              |
| `search.extern.BFSCtx`              | Semi-external BFS with context.                                              |
| `search.extern.PageRank`            | Semi-external PageRank against an mmap-backed csrfile.                       |
| `search.extern.PageRankCtx`         | Semi-external PageRank with context.                                         |

### `search/flow`

| Metric                                     | Description                                              |
| ------------------------------------------ | -------------------------------------------------------- |
| `search.flow.MaxFlow`                      | Dinic max-flow.                                          |
| `search.flow.MaxFlowCtx`                   | Dinic max-flow with context.                             |
| `search.flow.EdmondsKarp`                  | Edmonds-Karp max-flow (BFS-augmenting Ford-Fulkerson).   |
| `search.flow.EdmondsKarpCtx`               | Edmonds-Karp with context.                               |
| `search.flow.PushRelabelMaxFlow`           | FIFO push-relabel with the gap heuristic.                |
| `search.flow.PushRelabelMaxFlowCtx`        | Push-relabel max-flow with context.                      |
| `search.flow.MinCostMaxFlow`               | Successive shortest paths min-cost max-flow.             |
| `search.flow.MinCostMaxFlowCtx`            | Min-cost max-flow with context.                          |
| `search.flow.StoerWagner`                  | Stoer-Wagner global minimum cut.                         |
| `search.flow.StoerWagnerCtx`               | Stoer-Wagner with context.                               |

### `graph/io`

| Metric                              | Description                                                        |
| ----------------------------------- | ------------------------------------------------------------------ |
| `graph.io.csv.ReadInto`             | CSV edge-list reader.                                              |
| `graph.io.csv.ReadIntoCtx`          | CSV reader with context.                                           |
| `graph.io.csv.Write`                | CSV edge-list writer.                                              |
| `graph.io.csv.WriteCtx`             | CSV writer with context.                                           |
| `graph.io.dot.Write`                | Graphviz DOT writer.                                               |
| `graph.io.dot.WriteCtx`             | DOT writer with context.                                           |
| `graph.io.graphml.ReadInto`         | GraphML reader.                                                    |
| `graph.io.graphml.ReadIntoCtx`      | GraphML reader with context.                                       |
| `graph.io.graphml.Write`            | GraphML writer.                                                    |
| `graph.io.graphml.WriteCtx`         | GraphML writer with context.                                       |
| `graph.io.jsonl.ReadInto`           | JSON Lines reader.                                                 |
| `graph.io.jsonl.ReadIntoCtx`        | JSON Lines reader with context.                                    |
| `graph.io.jsonl.Write`              | JSON Lines writer.                                                 |
| `graph.io.jsonl.WriteCtx`           | JSON Lines writer with context.                                    |

### `store/wal`

| Metric                              | Description                                                        |
| ----------------------------------- | ------------------------------------------------------------------ |
| `store.wal.Open`                    | Open the WAL writer for append-only writes.                        |
| `store.wal.OpenReader`              | Open a WAL reader for iteration.                                   |
| `store.wal.Append`                  | Append one frame (synchronous wrapper).                            |
| `store.wal.AppendCtx`               | Append one frame with context.                                     |
| `store.wal.Sync`                    | fsync the WAL (synchronous wrapper).                               |
| `store.wal.SyncCtx`                 | fsync the WAL with context.                                        |
| `store.wal.Truncate`                | Truncate the WAL to zero length.                                   |
| `store.wal.Close`                   | Flush + Sync + close the WAL.                                      |
| `store.wal.Replay`                  | Iterate every frame applying a caller-supplied function.           |
| `store.wal.Encode`                  | Encode one frame to a writer.                                      |
| `store.wal.Decode`                  | Decode one frame from a reader.                                    |

### `store/snapshot`

| Metric                                  | Description                                                    |
| --------------------------------------- | -------------------------------------------------------------- |
| `store.snapshot.WriteCSR`               | Serialise a CSR to a snapshot writer.                          |
| `store.snapshot.ReadCSR`                | Parse a CSR from a snapshot reader.                            |
| `store.snapshot.WriteSnapshotCSR`       | Atomic snapshot publish (without context).                     |
| `store.snapshot.WriteSnapshotCSRCtx`    | Atomic snapshot publish with context.                          |
| `store.snapshot.Open`                   | Verify and load a snapshot directory.                          |
| `store.snapshot.WriteManifest`          | Write a snapshot manifest to an `io.Writer`.                   |
| `store.snapshot.LoadManifest`           | Parse a snapshot manifest from an `io.Reader`.                 |
| `store.snapshot.ReadManifestFile`       | Open + parse a manifest from disk.                             |
| `store.snapshot.WriteIndexes`           | Serialise every registered index under `indexes/<name>.bin`.   |
| `store.snapshot.LoadIndexes`            | Read every `indexes/<name>.bin` referenced by the manifest.    |
| `store.snapshot.indexes.loaded`         | Counter: number of indexes successfully re-hydrated.           |
| `store.snapshot.indexes.corrupted`      | Counter: number of indexes whose file was missing or CRC-bad.  |

### `store/txn`

| Metric                       | Description                                                           |
| ---------------------------- | --------------------------------------------------------------------- |
| `store.txn.Begin`            | Open a new transaction (synchronous wrapper).                         |
| `store.txn.BeginCtx`         | Open a new transaction with context.                                  |
| `store.txn.Commit`           | fsync-append every buffered op then apply to the graph.               |
| `store.txn.Rollback`         | Discard buffered ops without touching WAL or graph.                   |

### `store/checkpoint`

| Metric                                       | Description                                                              |
| -------------------------------------------- | ------------------------------------------------------------------------ |
| `store.checkpoint.Trigger`                   | Request a checkpoint (synchronous wrapper).                              |
| `store.checkpoint.TriggerCtx`                | Request a checkpoint with context.                                       |
| `store.checkpoint.wal_truncated_bytes`       | Counter: bytes reclaimed from the WAL prefix on each successful checkpoint. Emitted post-snapshot, post-truncate; the lifetime aggregate is also surfaced as `Stats.WALTruncBytes`. |

### `store/recovery`

| Metric                                | Description                                                      |
| ------------------------------------- | ---------------------------------------------------------------- |
| `store.recovery.Decode`               | Decode one transactional WAL payload.                            |
| `store.recovery.Open`                 | Snapshot+WAL recovery into a fresh graph (any key type).         |
| `store.recovery.OpenCtx`              | Context-aware recovery; honours cancellation and deadlines.      |

### `store/bulk`

| Metric                       | Description                                                           |
| ---------------------------- | --------------------------------------------------------------------- |
| `store.bulk.Add`             | Ingest one edge into the bulk loader.                                 |
| `store.bulk.AddBatch`        | Ingest a contiguous batch of edges.                                   |
| `store.bulk.Drain`           | Drain a channel of edges into the loader.                             |
| `store.bulk.Finalise`        | Build the CSR and write the csrfile.                                  |

## Error counters

Every metric listed above that can return a non-nil `error` has a
companion `<metric>.errors` counter incremented once per failing
return. The counter does not carry the cause; consumers that need
to distinguish errno-style breakdowns can pair the metric with
structured logs through `log/slog`.

The smoke test in `internal/metrics/wireup_test.go` exercises a
representative sample of both the latency and the error paths and
fails loudly when a wired symbol stops emitting its expected name.

## Backend integration

The default backend is a stateless no-op (`internal/metrics`
`noopBackend{}`). To wire Prometheus or OpenTelemetry, implement
the `Backend` interface in the consuming application and install
it via `metrics.SetBackend`:

```go
type promBackend struct {
    counters   *prometheus.CounterVec
    histograms *prometheus.HistogramVec
}

func (p *promBackend) IncCounter(name string, delta uint64) {
    p.counters.WithLabelValues(name).Add(float64(delta))
}

func (p *promBackend) ObserveLatency(name string, d time.Duration) {
    p.histograms.WithLabelValues(name).Observe(d.Seconds())
}

metrics.SetBackend(&promBackend{ /* ... */ })
```

`SetBackend(nil)` restores the no-op default. Backend swaps are
lock-free (`atomic.Pointer`), so a single global swap is safe even
under concurrent load.

## See also

* `docs/profiling.md` — when to use `pprof` versus the metrics
  inventory.
* `docs/benchmarks/` — long-running benchmark reports; metric
  overhead is included in every published `benchstat` run.
* CLAUDE.md, section "Observability" — the project-wide policy
  this inventory implements.
