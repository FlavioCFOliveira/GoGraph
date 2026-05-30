# Example 20 — Concurrent reads over an immutable CSR

## What it demonstrates

The lock-free read contract of a frozen `csr.CSR`: once a graph is
snapshotted into an immutable CSR, any number of goroutines may traverse
it simultaneously with **zero synchronisation on the snapshot itself**.
The example runs three different read-only algorithms — Dijkstra, BFS,
and PageRank — concurrently over a single shared CSR.

## Domain / scenario

A 100-node undirected graph wired as a ring with a chord: every node `i`
connects to `(i+1) % 100` and to `(i+7) % 100`, with small integer
weights. The graph is built once with the mutable `adjlist` builder, then
frozen into one `csr.CSR` snapshot that all three goroutines read:

- **Reader 1 — Dijkstra:** eight single-source shortest-path runs, one
  per seed `s` in `0..7`, each measuring the cost to its antipodal node
  `(s+50) % 100`; the costs are summed.
- **Reader 2 — BFS:** a breadth-first reach count from node 0.
- **Reader 3 — PageRank:** power iteration to convergence, counting the
  nodes that end up with a non-zero rank.

The CSR is never mutated after it is built, so the readers need no lock
on it. The only shared mutable state is the results map the goroutines
publish into, which is guarded by a `sync.Mutex`.

## How to run

```sh
go run ./examples/20_concurrent_reads
```

## Expected output

The goroutines finish in a non-deterministic order, but the report is
printed in a fixed key order and the reported aggregates are
deterministic for the hard-coded inputs. A representative run:

```
Concurrent results over a single immutable CSR:
  dijkstra  8 SSSPs, summed cost = 110
  bfs       BFS reached 100 nodes
  pagerank  PageRank 1 iters, 100 live ranks
```

The summed Dijkstra cost (110), the BFS reach count (100), and the
PageRank live-rank count (100) are stable across runs; only the
goroutine completion order varies. The PageRank iteration count is shown
for context.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable weighted undirected graph.
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
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
