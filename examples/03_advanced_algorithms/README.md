# Example 03 — Advanced algorithms

## What it demonstrates

Running four GoGraph algorithms over a single immutable CSR snapshot:
breadth-first traversal (`search.BFS`), single-source shortest paths
(`search.Dijkstra`), exact betweenness centrality
(`centrality.Betweenness`, Brandes' algorithm), and PageRank
(`centrality.PageRank`). It also shows the idiom for turning a
NodeID-indexed result slice into a stable, name-sorted report by
enumerating only the live nodes.

## Domain / scenario

A tiny undirected social graph of four people. Each edge is a mutual
link; the `alice -> carol` link is weighted 2, every other link 1:

```
alice -- bob   (1)
bob   -- carol (1)
carol -- dave  (1)
alice -- carol (2)
```

Carol is the structural hub: she sits on the only shortest path between
every other pair of people, so the interesting result is that her
betweenness score (4.0) and PageRank (the highest) both dominate. The
weighted `alice -> dave` shortest distance is 3
(`alice -> bob -> carol -> dave`, costing 1 + 1 + 1), beating the
`alice -> carol -> dave` route that costs 2 + 1 = 3 but is found via the
unit-weight chain.

## How to run

```sh
go run ./examples/03_advanced_algorithms
```

## Expected output

```
BFS from alice:
  alice at depth 0
  bob   at depth 1
  carol at depth 1
  dave  at depth 2
Dijkstra alice -> dave: 3
Betweenness centrality:
  alice 0.0000
  bob   0.0000
  carol 4.0000
  dave  0.0000
PageRank converged in 29 iterations:
  alice 0.2459
  bob   0.2459
  carol 0.3667
  dave  0.1414
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected weighted graph.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot shared by every algorithm.
- `graph/csr.CSR.LiveNodes` — enumerate only NodeIDs with an incident edge, skipping ghost slots from sharded packing.
- `search.BFS` — breadth-first traversal in non-decreasing depth order.
- `search.Dijkstra` — single-source shortest paths over non-negative weights.
- `search/centrality.Betweenness` — exact betweenness centrality via Brandes' algorithm.
- `search/centrality.PageRank` / `DefaultPageRankOptions` — stationary random-surfer importance scores.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`search/centrality`](../../search/centrality) — betweenness and PageRank package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the shared query surface
- [Example 01 — basic shortest paths](../01_basic) — the minimal Dijkstra flow this example builds on
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
