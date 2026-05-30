# Example 08 — PageRank

## What it demonstrates

Runs `search/centrality.PageRank` over a small directed graph and reads
back a per-node stationary-rank vector, then prints the pages ordered
from most to least important. It also shows how to filter the live
nodes out of the rank slice through the adjacency-list mapper.

## Domain / scenario

A tiny web of six pages that link to one another:

```
B -> A    H -> B
C -> A    H -> C
D -> A
E -> A    A -> H
```

Four peripheral pages (B, C, D, E) all link to a single **Authority**
page A, giving A a high in-degree. A's only out-link feeds a **Hub**
page H, and H endorses just two of the four peripheral pages (B and C).

This topology is deliberately *not* a symmetric cycle. In a directed
cycle every PageRank score is identical, so the ranks tell you nothing.
Here the asymmetry produces a clear gradient:

- **A** wins — every other page endorses it.
- **H** is second — it receives the authority's entire single out-link.
- **B** and **C** outrank **D** and **E** — they collect a share from
  the hub on top of the uniform teleport mass, while D and E receive
  only the teleport (random-jump) share.

B and C are symmetric, so they tie; likewise D and E. The example sorts
descending by rank and breaks ties by page name, so the printed order
is fully deterministic.

## How to run

```sh
go run ./examples/08_pagerank
```

## Expected output

```
Converged in 86 iterations (6 live pages)
  1. page A: 0.331875
  2. page H: 0.307095
  3. page B: 0.155515
  4. page C: 0.155515
  5. page D: 0.025000
  6. page E: 0.025000
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable directed graph.
- `graph/adjlist.AdjList.Mapper` — resolve page names to compact `NodeID`s (`Lookup`), used to read only the live entries from the rank slice.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for the analytics pass.
- `search/centrality.PageRank` — power-iteration PageRank returning the per-`NodeID` rank slice and the iteration count to convergence.
- `search/centrality.DefaultPageRankOptions` — the classic Brin-Page parameters (damping 0.85, max 100 iterations, tolerance 1e-6).

## Further reading

- [`search/centrality`](../../search/centrality) — centrality algorithms package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 01 — basic shortest paths](../01_basic) — the minimal build-snapshot-query flow this example extends
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
