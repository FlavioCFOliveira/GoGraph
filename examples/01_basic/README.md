# Example 01 — Basic shortest paths

## What it demonstrates

The minimal end-to-end GoGraph flow: build a weighted directed graph
with the mutable `adjlist` builder, freeze it into an immutable CSR
snapshot, and run a single-source shortest-paths query with
`search.Dijkstra`. It reads back both the shortest distance and the
reconstructed route to each destination.

## Domain / scenario

A tiny road network between five European cities. Each directed edge is
a one-way leg with a distance in kilometres:

```
Lisbon -> Madrid (624)
Lisbon -> Paris  (1737)
Madrid -> Paris  (1274)
Madrid -> Rome   (1969)
Paris  -> Rome   (1422)
```

The query is anchored at `Lisbon`. The interesting case is `Rome`:
the direct-looking `Lisbon -> Paris -> Rome` path costs 3159 km, while
`Lisbon -> Madrid -> Rome` costs only 2593 km — so the route, not just
the distance, is worth printing.

## How to run

```sh
go run ./examples/01_basic
```

## Expected output

```
Lisbon -> Madrid  :  624 km   route: Lisbon -> Madrid
Lisbon -> Paris   : 1737 km   route: Lisbon -> Paris
Lisbon -> Rome    : 2593 km   route: Lisbon -> Madrid -> Rome
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable weighted directed graph.
- `graph/adjlist.AdjList.Mapper` — translate between city names and compact `NodeID`s (`Lookup`, `Resolve`).
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `search.Dijkstra` — single-source shortest paths over non-negative weights.
- `search.Distances.Distance` / `search.Distances.Path` — read back the cost and reconstruct the route to each node.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 02 — property graph](../02_property_graph) — the next example, building on this flow
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
