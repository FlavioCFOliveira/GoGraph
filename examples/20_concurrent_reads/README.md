# Example 20 — Concurrent reads over an immutable CSR

## What it demonstrates

The lock-free read contract of a frozen `csr.CSR`: once a graph is
snapshotted into an immutable CSR, any number of goroutines may traverse
it simultaneously with **zero synchronisation on the snapshot itself**.
A pool of worker goroutines runs the same mixed read workload — Dijkstra,
BFS, and PageRank — concurrently over one shared CSR, and the example
measures how read throughput scales with the worker count while proving
that every concurrent read returns the same answer as a single-threaded
read.

## Domain / scenario

A **Barabási-Albert preferential-attachment** network — the canonical
model of a social / web graph, where a few high-degree hubs dominate and
the degree distribution is heavy-tailed. The seeded generator builds it
single-threaded so the RNG draws in a fixed order:

- It starts from a connected path core of `seed-core` nodes.
- Each subsequent node attaches `attach` edges to existing nodes chosen
  with probability proportional to their current degree (the standard
  repeated-node-list method), rejecting self-loops and parallel targets.
- Every edge carries an integer weight in `[1, weight-max]`.

Because every new node attaches to the already-connected component, the
graph is **connected by construction** — so BFS reaches every node and its
reach count is a constant. The hub structure gives PageRank a
well-separated top-k, and the high-degree fan-out makes each read do real
CPU work. Integer weights keep Dijkstra free of NaN/Inf concerns and make
distance sums exact.

The graph is built once into a mutable `adjlist`, then frozen into a
single `csr.CSR` snapshot that every worker reads. The CSR is never
mutated after it is built, so the readers need no lock on it. The only
shared mutable state is per-level bookkeeping (an atomic read counter and
an atomic mismatch flag) — never the snapshot.

## How to run

```sh
go run ./examples/20_concurrent_reads                          # small deterministic default
go run ./examples/20_concurrent_reads -nodes 200000 -attach 8 -workers 16  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large example |
|---|---|---|---|
| `-nodes` | number of nodes in the scale-free network | `4000` | `200000` |
| `-attach` | BA attachment degree `m` (edges each new node adds) | `4` | `8` |
| `-seed-core` | size of the connected seed core (must exceed `-attach`) | `8` | `16` |
| `-weight-max` | edge weights are drawn from `[1, weight-max]` | `10` | `100` |
| `-workers` | maximum worker count the scaling sweep climbs to | `8` | `16` |
| `-iterations` | Dijkstra SSSPs each worker runs per round | `16` | `64` |
| `-top-k` | PageRank top-k set size pinned as an invariant | `10` | `10` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `1` | any |

The sweep climbs worker counts `1, 2, 4, 8, …` capped at both `-workers`
and `GOMAXPROCS`. The default completes in well under a second; the large
run does enough work per read for the scaling curve to be clearly
observable.

## Expected output

The deterministic **fact** lines below are stable for a fixed `-seed`. The
`# `-prefixed **telemetry** lines (heap, throughput, durations) vary per
run and per machine and are never pinned by the test.

```
config.nodes=4000
config.attach=4
config.seed_core=8
config.weight_max=10
config.iterations=16
config.top_k=10
config.seed=1
nodes.count=4000
edges.directed=31950
# mem.heap_alloc=755.83 KiB
# mem.heap_growth=552.22 KiB
# mem.num_gc=2
ref.dijkstra_dist=17
ref.bfs_reached=4000
ref.pagerank_topk=[230,7,73,65,15,213,32,205,139,165]
# scale.workers_1.reads=16
# scale.workers_1.elapsed=27.229ms
# scale.workers_1.throughput=588 reads/s
# scale.workers_2.throughput=1124 reads/s
# scale.workers_4.throughput=1638 reads/s
# scale.workers_8.throughput=2312 reads/s
reads.agree=true
```

`ref.bfs_reached` equals `nodes.count` (the graph is connected),
`ref.dijkstra_dist` and `ref.pagerank_topk` are reproducible for the
seed, and `reads.agree=true` is the headline correctness fact: every
concurrent read at every worker count returned the same answer as the
single-threaded reference.

## Evidence it collects

This is a **concurrency** example, so it reports (per the taxonomy in
[`docs/examples-standard.md`](../../docs/examples-standard.md)):

- **Aggregate throughput** — reads/s of the mixed workload at each level.
- **Per-worker-count scaling** — the identical workload run at `1, 2, 4,
  8 …` workers; throughput that climbs with the worker count is the
  observable evidence that readers do not contend on the snapshot. Scale
  `-nodes`/`-attach` up and watch the curve: it should keep climbing until
  the worker count approaches `GOMAXPROCS`.
- **Live heap** — one immutable snapshot is shared, not copied per worker,
  so the heap does not grow with the worker count.

The correctness evidence — `reads.agree=true` plus the constant
`ref.*` facts — is the proof that the lock-free read path is sound: many
concurrent readers compute exactly what one reader computes.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable weighted undirected graph.
- `graph/adjlist.AdjList.Mapper` — resolve node values to stable `NodeID`s for the fixed source/target.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot, the shared read surface for all goroutines.
- `search.Dijkstra` — single-source shortest paths; safe to call concurrently on a snapshot CSR.
- `search.BFS` — breadth-first traversal with a visit callback; allocation-free on the hot path after the first call.
- `search/centrality.PageRank` / `DefaultPageRankOptions` — power-iteration PageRank, safe to invoke from any number of goroutines on a snapshot CSR.

## Further reading

- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot and its concurrent-read contract
- [`search`](../../search) — Dijkstra and BFS package documentation
- [`search/centrality`](../../search/centrality) — PageRank and other centrality measures
- [Example 01 — basic shortest paths](../01_basic) — the single-threaded build-snapshot-query flow this example parallelises
- [Example 08 — PageRank](../08_pagerank) — PageRank in isolation
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference end-state example this one follows
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
