# Example 14 — Routing alternatives

## What it demonstrates

Three flavours of shortest-path computation over one routing graph:
classical single-source `search.Dijkstra`, Yen's k-shortest paths for
ranked alternatives (`search.YenKShortest`), and `search.AStar` driven
by a **coordinate-based Euclidean heuristic**. The A* section is the
focus: it shows how an admissible heuristic lets A* reach the same
optimal answer as Dijkstra while expanding fewer nodes.

## Domain / scenario

A small directed road network between six European cities, weighted by
road distance in kilometres:

```
lisbon    -> madrid     (624)
lisbon    -> porto      (313)
porto     -> madrid     (568)
madrid    -> barcelona  (622)
porto     -> barcelona  (1200)
madrid    -> paris      (1274)
barcelona -> paris      (1000)
paris     -> berlin     (1054)
barcelona -> berlin     (1500)
```

Each city is also given a fixed 2D coordinate. The coordinates are an
input to the example, not derived from the edge weights, so the
heuristic is independent of the costs it is guiding.

## The heuristic, and why it is admissible

A* orders its frontier by `f(n) = g(n) + h(n)`, where `g(n)` is the
cost already paid to reach `n` and `h(n)` estimates the remaining cost
to the destination. The estimate used here is the **straight-line
(Euclidean) distance** between a node's coordinate and the
destination's coordinate, multiplied by a constant scale:

```
h(n) = floor(scale * euclidean(coord[n], coord[dst]))
```

For A* to return the *optimal* path, `h` must be **admissible**: it may
never overestimate the true remaining cost, i.e. `h(n) <=
true_remaining_road_cost(n)` for every `n`. The coordinates in this
example are laid out so that each city's straight-line distance to
`berlin` is already a lower bound on its true shortest remaining road
distance, so the scale is `1.0` and the `floor` only tightens the bound
further. Because a constant scale applied to a Euclidean metric also
satisfies `h(u) <= w(u, v) + h(v)` for every edge `u -> v`, the
heuristic is additionally **consistent**, so A* never re-expands a
settled node.

Admissibility is not asserted from theory alone: `example_test.go`
verifies it empirically by checking that A*'s returned cost equals
Dijkstra's cost for several source/destination pairs. If a future
coordinate change made the heuristic overestimate, those costs would
diverge and the test would fail.

## A* versus Dijkstra: the expansion count

Dijkstra is exactly A* with `h(n) = 0`. Without a heuristic it has no
sense of direction and settles every node whose distance is below the
destination's. The coordinate heuristic biases the frontier towards
`berlin`, so A* settles only the nodes on or near the optimal corridor.

On the `lisbon -> berlin` query Dijkstra settles all **6** nodes, while
A* settles **4** (`lisbon -> madrid -> barcelona -> berlin`) — the
`porto` and `paris` detours are pruned. Both return the same optimal
cost of 2746 km. The example reports both counts so the speed-up is
visible, and cross-checks the instrumented count against the library
result so the two cannot drift.

## How to run

```sh
go run ./examples/14_routing_alternatives
```

## Expected output

```
Dijkstra lisbon -> berlin: 2746 km
  route: lisbon -> madrid -> barcelona -> berlin

Yen's 3 shortest paths lisbon -> berlin:
  1. 2746 km via lisbon -> madrid -> barcelona -> berlin
  2. 2952 km via lisbon -> madrid -> paris -> berlin
  3. 3003 km via lisbon -> porto -> madrid -> barcelona -> berlin

A* lisbon -> berlin (coordinate-based Euclidean heuristic):
  cost = 2746 km, 3 hops
  route: lisbon -> madrid -> barcelona -> berlin

Nodes expanded (lower is better):
  Dijkstra (zero heuristic) : 6
  A* (Euclidean heuristic)  : 4
  same optimal cost: true (2746 km)
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable weighted directed road graph.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for queries.
- `graph/csr.CSR.NeighboursByID` — public out-neighbour iterator used by the expansion counter.
- `search.Dijkstra` / `search.Distances.Distance` / `search.Distances.Path` — single-source shortest paths and route read-back.
- `search.YenKShortest` — ranked loopless k-shortest alternatives between two nodes.
- `search.AStar` — point-to-point shortest path guided by a caller-supplied admissible heuristic.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [Example 01 — basic shortest paths](../01_basic) — the minimal Dijkstra flow this example builds on
- [Example 10 — DIMACS-9 routing](../10_dimacs9_routing) — shortest-path routing at dataset scale
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```

