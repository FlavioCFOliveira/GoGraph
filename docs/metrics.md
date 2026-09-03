# Metrics Inventory

This document enumerates every observability metric exported by
GoGraph's public blocking APIs. It is the authoritative companion
to the `internal/metrics` package and the CLAUDE.md mandate on
"latency histograms on every public blocking API".

The metrics are emitted through the [`metrics.Backend`][pubmetrics]
interface, exposed by the public `github.com/FlavioCFOliveira/GoGraph/metrics`
package. The default backend is a no-op; the cost of an
unconfigured metric site is two atomic loads and one `time.Now()`
pair (~50ns per call site). Installing a `Backend` via
`metrics.SetBackend` activates dispatch. A Prometheus-compatible
backend lives outside the dependency graph: any consumer that
implements the `Backend` interface can wire GoGraph into its own
`prometheus.Registry` without forcing `prometheus/client_golang`
into the module graph.

[pubmetrics]: https://pkg.go.dev/github.com/FlavioCFOliveira/GoGraph/metrics

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
* **Prometheus backend (in-tree, `metrics.NewPrometheusRegistry`)**:
  measured against the no-op default on the same operation mix
  (a counter every operation, a latency observation on one in four,
  a gauge on one in sixteen), Apple M4, Go 1.27.1, by
  `BenchmarkEmit` / `BenchmarkEmitParallel` in
  `internal/metrics/emit_backend_bench_test.go`:

  | goroutines | no-op | Prometheus | enabled cost |
  |---|---|---|---|
  | 1 | 4.28 ns/op | 16.5 ns/op | +12.2 ns/op (3.9x) |
  | 8 | 0.83 ns/op | 4.34 ns/op | +3.5 ns/op (5.2x) |
  | 64 | 0.70 ns/op | 4.13 ns/op | +3.4 ns/op (5.9x) |

  Enabling the backend therefore costs roughly 12 ns per emission on
  one goroutine, and the cost per emission FALLS as goroutines are
  added because emission scales with the cores. Reproduce with:

  ```
  go test -run='^$' -bench=BenchmarkEmit -benchmem -cpu=1,8,64 -count=6 ./internal/metrics/
  ```

  A consumer that wires its own backend instead pays whatever that
  implementation costs; the numbers above describe the in-tree one.

### Emission under concurrency

A counter is one atomic word, so several goroutines incrementing the
same series would serialise on one cache line — and did: before
rmp #2698 the in-tree registry ran 47.6 ns/op at 8 goroutines against
8.35 ns/op at one, a SCALING of 0.18x, meaning adding cores made
emission slower in absolute terms. That is a defect against the
extreme-concurrency mandate rather than a tuning opportunity.

A series now grows per-core accumulators the first time two goroutines
are observed emitting to it concurrently, and is summed at scrape time.
The measured effect on `BenchmarkIncCounterParallel` is 47.6 -> 1.70 ns/op
at 8 goroutines and 50.2 -> 1.76 ns/op at 64, against a cost of
+13% on the uncontended single-goroutine path (8.35 -> 9.46 ns/op).

Nothing about the exposition changes: the same names, labels, types and
values are produced whether or not a series has been promoted, which
`TestExposition_IdenticalWhetherPromoted` asserts byte-for-byte.

The memory is paid only by series that are actually contended. Each
promoted series costs 4 KiB; the unconditional cost is one pointer per
series, 3,920 bytes across the module's full cardinality of 268 counters
and 222 histograms. See `internal/metrics/prometheus/striped.go` for the
design and `footprint_test.go` for the measurement.

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

Each logical IO operation records exactly one latency sample, under its
base operation name. The context-aware variant (e.g. `WriteCtx`,
`ReadIntoCappedCtx`) owns the timer; the convenience wrapper that
supplies `context.Background()` delegates to it without timing again, so
a call records a single sample regardless of entry point (rmp #1524).

| Metric                              | Description                                                        |
| ----------------------------------- | ------------------------------------------------------------------ |
| `graph.io.csv.ReadInto`             | CSV edge-list reader.                                              |
| `graph.io.csv.Write`                | CSV edge-list writer.                                              |
| `graph.io.dot.Write`                | Graphviz DOT writer.                                               |
| `graph.io.graphml.ReadInto`         | GraphML reader.                                                    |
| `graph.io.graphml.ReadWithProps`    | GraphML property-graph reader.                                    |
| `graph.io.graphml.Write`            | GraphML writer.                                                    |
| `graph.io.graphml.WriteWithProps`   | GraphML property-graph writer.                                    |
| `graph.io.jsonl.ReadInto`           | JSON Lines reader.                                                 |
| `graph.io.jsonl.ReadWithProps`      | JSON Lines property-graph reader.                                 |
| `graph.io.jsonl.Write`              | JSON Lines writer.                                                 |
| `graph.io.jsonl.WriteWithProps`     | JSON Lines property-graph writer.                                 |

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
| `store.snapshot.indexes.written`         | Counter: number of index payloads a snapshot publish wrote.    |
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

The counters below are emitted by the **engine** (`cypher`) as it re-registers
each recovered secondary index, not by `store/recovery` itself, which loads none.
They name the recovery event rather than the emitting package so an operator can
read a reopen's index work as one group. See "Recovery semantics" in
[`persistence.md`](persistence.md).

| Metric                                       | Description                                                                    |
| -------------------------------------------- | ------------------------------------------------------------------------------ |
| `store.recovery.indexes.hydrated`            | Counter: indexes populated from a snapshot `indexes/<name>.bin` payload.        |
| `store.recovery.indexes.rebuilt`             | Counter: indexes populated by scanning the recovered graph instead.             |
| `store.recovery.indexes.backfill_nodes`      | Counter: node references those rebuilds materialised (the work hydration saves).|
| `store.recovery.indexes.payload_unreadable`  | Counter: payloads recovery reported unreadable (missing file or CRC mismatch).  |
| `store.recovery.indexes.payload_corrupted`   | Counter: CRC-valid payloads whose own `Deserialize` rejected them.              |

### `store/bulk`

| Metric                       | Description                                                           |
| ---------------------------- | --------------------------------------------------------------------- |
| `store.bulk.Add`             | Ingest one edge into the bulk loader.                                 |
| `store.bulk.AddBatch`        | Ingest a contiguous batch of edges.                                   |
| `store.bulk.Drain`           | Drain a channel of edges into the loader.                             |
| `store.bulk.Finalise`        | Build the CSR and write the csrfile.                                  |

### `cypher`

| Metric                    | Description                                                                     |
| ------------------------- | ------------------------------------------------------------------------------- |
| `cypher.Run`              | Full latency of `Engine.Run` (parse + plan + materialise) per query.            |
| `cypher.RunInTx`          | Full latency of `Engine.RunInTx` (parse + plan + write-tx materialise).         |
| `cypher.result.leaked`    | Count of `*Result` values collected by the GC without an explicit `Close` call. |

Plan-cache event counters (no latency dimension; incremented as raw counters):

| Counter                             | Description                                     |
| ----------------------------------- | ----------------------------------------------- |
| `cypher.plan_cache.hits`            | Cached plan returned without re-planning.        |
| `cypher.plan_cache.misses`          | Cache miss — plan compiled from scratch.         |
| `cypher.plan_cache.evictions`       | Entry evicted from the bounded LRU plan cache.   |
| `cypher.plan_cache.invalidations`   | Entry invalidated by a schema change (DDL).      |

### Pool utilisation counters

Every named `sync.Pool` emits a `get` and `put` counter so operators can observe
pool activity rate and detect pressure (high get/put ratio indicates pool is not
retaining objects between calls):

| Counter | Description |
| --- | --- |
| `search.pool.bfs.get` / `.put` | `bfsPool` acquire / release (BFS, bidirectional BFS, A*). |
| `search.pool.dfs.get` / `.put` | `dfsPool` acquire / release (iterative DFS). |
| `search.pool.bfs_do.get` / `.put` | `bfsDoScratchPool` acquire / release (direction-optimising BFS). |
| `search.pool.dijkstra.get` / `.put` | `dijkstraPool[W]` acquire / release (all Dijkstra variants). |
| `cypher.pool.slab.get` / `.put` | `SlabPool` acquire / release (Cypher row-slab allocator). |
| `bolt.pool.encoder.get` / `.put` | `EncodePool` acquire / release (Bolt PackStream encoder). |
| `bolt.pool.decoder.get` / `.put` | `DecodePool` acquire / release (Bolt PackStream decoder). |

### `graph/lpg` — the MVCC substrate

Multi-version concurrency control is GoGraph's **only** concurrency-control
mechanism, so the health of the MVCC substrate is the health of the module. These
series are what makes it observable.

Two naming shapes appear here, and the difference is deliberate:

* the **latency** series follow the module-wide
  `<package-path>.<ExportedSymbol>` convention, because they instrument public
  blocking APIs exactly as every other entry in this document does;
* the **substrate** series are named `lpg.mvcc.<quantity>`, because they describe a
  mechanism rather than a call. They are gauges and counters published from the
  background vacuum, not from any request path, so the workload they describe does
  not pay for them.

Bucketed series carry the bucket in the NAME (`…store.node_labels`,
`…bucket.4_7`) rather than in a Prometheus label: `metrics.Backend` takes a name
and a value and has no label dimension. Every such name is drawn from a closed set
and built once at init, so cardinality is bounded by construction and publication
allocates nothing.

#### Commit latency

| Metric | Description |
| --- | --- |
| `graph.lpg.ApplyVersioned` | One autocommit write transaction, open to publish. |
| `graph.lpg.ApplyVersionedCtx` | As above, with a context-bounded barrier acquisition. |
| `graph.lpg.EndVersionedTx` | The publish of a multi-statement transaction. |
| `lpg.mvcc.vacuum.pass` | One reclamation pass, start to finish. |

The durable commit is measured a layer up, by `store.txn.Commit` and
`store.txn.CommitWALOnly`; the client-visible one by `cypher.ExplicitTx.Commit` and
`cypher.RunInTx`.

#### Write outcomes

`Commits` and `Aborts` are the two outcomes and partition the transactions that
reached one. `Conflicts` is a CAUSE and is a **subset of aborts**, not a third
bucket — the same relationship `pg_stat_database` has between `xact_rollback` and
its conflict counters. A transaction that versioned nothing and hit no conflict is
in neither: there was no decision to record.

| Metric | Kind | Description |
| --- | --- | --- |
| `lpg.mvcc.writers.active` | gauge | Write transactions in flight. |
| `lpg.mvcc.commits` | gauge | Transactions that published an instant. |
| `lpg.mvcc.aborts` | gauge | Transactions refused publication. |
| `lpg.mvcc.conflicts` | counter | One per DOOMED TRANSACTION, at the detection site. |
| `lpg.mvcc.conflicts.store.<store>` | counter | The same, attributed to the store that refused it. |
| `lpg.mvcc.conflicts.total` | gauge | Cumulative conflicts, sampled with the commit count so the two can be divided without racing two scrapes. |
| `lpg.mvcc.conflicts.total.store.<store>` | gauge | Cumulative conflicts per store. |
| `lpg.mvcc.conflict_rate` | gauge | `conflicts / (commits + aborts)`. |

`<store>` is one of `node_labels`, `node_properties`, `node_existence`,
`adjacency`, `edge_types`, `edge_types_by_handle`, `edge_types_by_ordinal`,
`edge_props_by_handle`, `edge_props_by_ordinal`, `other`.

#### Retention and the reclamation horizon

| Metric | Kind | Description |
| --- | --- | --- |
| `lpg.mvcc.versions.{label,property,adjacency,edge_side,node_life}` | gauge | Live version records per store. |
| `lpg.mvcc.versions.total` | gauge | Their sum: the memory the substrate is responsible for. |
| `lpg.mvcc.versions.bound` | gauge | The churn bound the settled state returns to. |
| `lpg.mvcc.versions.ceiling` | gauge | The instantaneous bound a committer waits at. |
| `lpg.mvcc.index_removal_backlog` | gauge | Label-index removals waiting for the watermark. |
| `lpg.mvcc.adjacency_conflict_stamps` | gauge | Nodes carrying an adjacency conflict stamp. |
| `lpg.mvcc.watermark` | gauge | The oldest start timestamp any active snapshot holds. |
| `lpg.mvcc.oldest_snapshot_age` | gauge | `now - watermark`. This IS the watermark age; it is published once, under one name. |
| `lpg.mvcc.in_flight_commits` | gauge | Timestamps allocated but not yet published. |
| `lpg.mvcc.sessions.waiting` | gauge | Sessions blocked waiting for the frontier to reach their own last commit. Read with `in_flight_commits`: those commits are what is holding the frontier back. |
| `lpg.mvcc.snapshots.active` | gauge | Snapshots registered with the horizon — readers AND writers. |
| `lpg.mvcc.readers.active` | gauge | `snapshots.active - writers.active`. |
| `lpg.mvcc.snapshots.unregistered` | gauge | Snapshots that could not get a slot. **While this is non-zero reclamation is SUSPENDED** and version memory has no bound. |
| `lpg.mvcc.snapshots.capacity` | gauge | The slot capacity the two above should be read against. |
| `lpg.mvcc.horizon.capacity` | gauge | The same constant, published beside the vacuum's series. |

#### Version-chain depth

Chain depth is read cost: a read steps back through each record newer than its own
instant. The distribution is what matters, because one object with a chain of 200 is
a latency spike that a mean over a million short chains reports as 1.0002. The
buckets hold **retained** depth — what a read arriving now would walk — measured by
the reclaimer during the walk it already performs.

Each store's contribution describes that store's most recent complete sweep.

| Metric | Kind | Description |
| --- | --- | --- |
| `lpg.mvcc.chain_depth.bucket.<b>` | gauge | Chains whose retained depth is in bucket `<b>`: `1`, `2_3`, `4_7`, `8_15`, `16_31`, `32_63`, `64_127`, `128_inf`. |
| `lpg.mvcc.chain_depth.chains` | gauge | Chains counted, over every store. |
| `lpg.mvcc.chain_depth.deepest` | gauge | The exact deepest retained chain — the last bucket is unbounded above, so without this a chain of 130 and one of 5000 read alike. |
| `lpg.mvcc.chain_depth.{chains,deepest}.store.<s>` | gauge | The same, per store: `node_labels`, `node_properties`, `edge_sides`, `adjacency`. |

Node existence has no entry: it is versioned, but at most one birth and one death
per node, so it keeps no chain. Its retention is `lpg.mvcc.versions.node_life`.

#### The background vacuum

| Metric | Kind | Description |
| --- | --- | --- |
| `lpg.mvcc.vacuum.running` | gauge | 1 while a sweeper goroutine is alive. |
| `lpg.mvcc.vacuum.starts` / `.exits` | gauge | Sweeper lifecycle. They differ by at most one. |
| `lpg.mvcc.vacuum.passes` | gauge | Completed passes. |
| `lpg.mvcc.vacuum.reclaimed` | gauge | Records released in total. |
| `lpg.mvcc.vacuum.capped_passes` | gauge | Passes that stopped at the per-pass record bound. |
| `lpg.mvcc.vacuum.backlog` | gauge | Versions created since the last pass began. |
| `lpg.mvcc.vacuum.records_per_pass` | gauge | The per-pass upper bound on work. |
| `lpg.mvcc.vacuum.pass_mean_ns` | gauge | Mean pass duration, for a consumer with no histogram backend. |
| `lpg.mvcc.vacuum.pressure_unrelieved` | counter | A committer waited at the ceiling and the sweep freed nothing. |

The measured cost of all of the above is recorded in
[`benchmarks/mvcc-observability-2026-08-05.md`](benchmarks/mvcc-observability-2026-08-05.md):
the read path is unchanged, and a write transaction pays two atomic increments —
1.42 ns on an otherwise empty bracket, unmeasurable on one that writes.

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

The default backend is a stateless no-op. The module ships a
ready-to-use Prometheus text-exposition-format backend through the
public `github.com/FlavioCFOliveira/GoGraph/metrics` package with no
external dependencies:

```go
import (
    "net/http"
    "github.com/FlavioCFOliveira/GoGraph/metrics"
)

reg := metrics.NewPrometheusRegistry()
metrics.SetBackend(reg)

// Expose /metrics endpoint:
http.Handle("/metrics", reg.Handler())
http.ListenAndServe(":9090", nil)
```

`reg.WriteText(w)` outputs all counters and latency histograms in
Prometheus text format (standard latency buckets: 100 µs to 5 s).
Metric names follow the convention `<package>_<symbol>` with dots
and slashes converted to underscores.

To integrate with `prometheus/client_golang` or OpenTelemetry instead,
implement the `Backend` interface directly and install it via
`metrics.SetBackend`:

```go
import (
    "time"
    "github.com/FlavioCFOliveira/GoGraph/metrics"
)

type myBackend struct{}

func (b *myBackend) IncCounter(name string, delta uint64)        { /* ... */ }
func (b *myBackend) ObserveLatency(name string, d time.Duration) { /* ... */ }

metrics.SetBackend(&myBackend{})
```

`metrics.SetBackend(nil)` restores the no-op default. Backend swaps are
lock-free (`atomic.Pointer`), so a single global swap is safe even
under concurrent load.

## See also

* `docs/profiling.md` — when to use `pprof` versus the metrics
  inventory.
* `docs/benchmarks/` — long-running benchmark reports; metric
  overhead is included in every published `benchstat` run.
* CLAUDE.md, section "Observability" — the project-wide policy
  this inventory implements.
