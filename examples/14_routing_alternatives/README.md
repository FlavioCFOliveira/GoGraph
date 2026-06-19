# Example 14 — Routing alternatives

## What it demonstrates

Three flavours of shortest-path computation over **one** seeded coordinate
routing graph: classical single-source `search.Dijkstra`, Yen's k-shortest
loopless paths for ranked alternatives (`search.YenKShortest`), and
`search.AStar` driven by a **coordinate-based Euclidean heuristic**. The A*
section is the focus, and the evidence the example collects is the
per-algorithm **nodes-expanded** count: an admissible, consistent heuristic
lets A* reach the same optimal answer as Dijkstra while settling
**dramatically fewer nodes**.

## Domain / scenario

A two-way road network laid out on a plane. The dataset is a **seeded
k-nearest-neighbour (k-NN) spatial graph**: `-nodes` points are drawn
uniformly at random from `[0, scale]²`, and each point is linked to its `k`
(`-neighbours`) nearest neighbours by an edge weighted with the integer
Euclidean distance between them. Each undirected road is stored as two
opposite directed arcs. A deterministic component-merge repair pass then adds
the fewest extra edges needed to guarantee a single connected component for
**any** seed, so the source can always reach the destination. The source is
the node nearest the `(0,0)` corner and the destination the node nearest the
`(scale,scale)` corner, which maximises the path length and therefore the A*
pruning advantage.

Fixing `-seed` fixes the coordinates, the neighbour edges, and therefore every
shortest path exactly.

## The heuristic, and why it is admissible

A* orders its frontier by `f(n) = g(n) + h(n)`, where `g(n)` is the cost
already paid to reach `n` and `h(n)` estimates the remaining cost to the
destination. With integer (`int64`) weights the example uses:

```
edge weight  w(u,v) = max(1, ceil(D(u,v)))   // CEIL  -> w >= true distance
heuristic    h(n)   = floor(D(n, dst))         // FLOOR -> h <= true distance
```

where `D(a,b)` is the Euclidean distance between coordinates.

- **Admissible:** `h(n) = floor(D(n,dst)) <= D(n,dst) <=` the sum of real edge
  lengths on any `n -> dst` path (a polyline is never shorter than its
  straight chord) `<=` the sum of `ceil(...)` = the integer path cost. So `h`
  never overestimates the true remaining cost, which is what guarantees A*
  returns the *optimal* path.
- **Consistent:** for every edge `u -> v`,
  `h(u) = floor(D(u,dst)) <= D(u,dst) <= D(u,v) + D(v,dst) <= ceil(D(u,v)) +
  D(v,dst) < ceil(D(u,v)) + floor(D(v,dst)) + 1`. All three terms are integers,
  so the strict `< (...) + 1` collapses to `<=`, giving
  `h(u) <= w(u,v) + h(v)`. A consistent heuristic never re-expands a settled
  node.

Rounding the edge **up** (ceil) and the heuristic **down** (floor) is the
correct pairing: rounding an edge down could make `w(u,v) < D(u,v)` and break
the lower-bound chain. The `max(1, .)` clamp only raises `w`, which only
relaxes the consistency inequality, so it is safe and avoids degenerate
zero-cost hops. `h(dst) = floor(0) = 0`, as A* requires.

Admissibility is not asserted from theory alone: `example_test.go` verifies it
empirically by checking that A*'s returned cost equals Dijkstra's cost. If a
future change made the heuristic overestimate, those costs would diverge and
the test would fail.

## A* versus Dijkstra: the expansion count

Dijkstra is exactly A* with `h(n) = 0`. Without a heuristic it has no sense of
direction and settles every node whose distance is below the destination's.
The coordinate heuristic biases the frontier towards the destination, so A*
settles only the nodes on or near the optimal corridor. Because the k-NN edges
are short and local, the optimal route is a long chain of small hops, so the
pruning is large.

On the default graph Dijkstra settles all **400** nodes, while A* settles only
**102** — about a quarter — and both return the same optimal cost. The example
reports both counts (and the saved count and fraction) as telemetry, and
cross-checks the instrumented count against the library result so the two
cannot drift. The `search` package exposes no native nodes-expanded counter,
so the count comes from a faithful, deterministic re-implementation of the
engine's settle order over the public `NeighboursByID` API.

## How to run

```sh
go run ./examples/14_routing_alternatives                       # small deterministic default
go run ./examples/14_routing_alternatives -nodes 200000 -seed 7 # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-nodes` | number of coordinate nodes generated | `400` | `200000` |
| `-neighbours` | k-NN degree: nearest neighbours each node links to | `8` | `10` |
| `-scale` | coordinate extent; points are drawn from `[0,scale]²` | `1000` | `100000` |
| `-k` | Yen's k: number of ranked shortest-path alternatives | `3` | `10` |
| `-seed` | RNG seed; fixes the coordinates and the whole graph shape | `1` | any |

## Expected output

```
config.nodes=400
config.neighbours=8
config.scale=1000
config.k=3
config.seed=1
graph.nodes=400
graph.edges=3744
graph.src=56
graph.dst=264
dijkstra.cost=1396
yen.count=3
yen.cost.1=1396
yen.cost.2=1396
yen.cost.3=1397
astar.cost=1396
astar.hops=17
astar_cost_equals_dijkstra=true
```

The lines above are the **deterministic facts** the regression test pins for a
fixed seed. The example also prints **telemetry** lines prefixed with `# ` —
for example:

```
# build.elapsed=20.6ms
# mem.heap_alloc=482.66 KiB
# dijkstra.latency=37µs
# expand.dijkstra=400
# expand.astar=102
# expand.astar_saved=298
# expand.astar_fraction=0.255
```

Telemetry varies per run and per machine and is never asserted. The
`expand.*` lines are the headline evidence: A* expands 102 nodes against
Dijkstra's 400 for the identical optimal cost.

## Evidence it collects

This is a **search / path-finding** example, so it reports the dimensions the
standard's taxonomy lists for that subject:

- **Wall-clock latency per query** — `# dijkstra.latency`, `# yen.latency`,
  `# astar.latency`.
- **Nodes settled / expanded** — `# expand.dijkstra`, `# expand.astar`, and
  the derived `# expand.astar_saved` / `# expand.astar_fraction`, which
  quantify the A*-vs-Dijkstra advantage that is the example's whole point.
- **Live heap and bytes per edge** — `# mem.heap_alloc`,
  `# mem.heap_growth`, `# mem.bytes_per_edge` after a forced GC.

Scale it up with `-nodes` and watch the expansion gap and the timings grow:
the absolute number of nodes A* skips increases with the corridor length, and
the build/heap figures show the in-memory footprint of a larger CSR snapshot.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` / `AdjList.Compact` — build, then
  tighten, the mutable weighted directed road graph.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR
  snapshot for queries.
- `graph/csr.CSR.NeighboursByID` — public out-neighbour iterator used by the
  expansion counter.
- `graph.Mapper.Lookup` / `graph.Mapper.Resolve` — translate between
  coordinate indices and the (hash-scattered) graph NodeIDs.
- `search.DijkstraCtx` / `search.Distances.Distance` — single-source shortest
  paths and distance read-back.
- `search.YenKShortestCtx` — ranked loopless k-shortest alternatives between
  two nodes, returned in ascending cost order.
- `search.AStarCtx` — point-to-point shortest path guided by a
  caller-supplied admissible heuristic.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [Example 01 — basic shortest paths](../01_basic) — the minimal Dijkstra flow this example builds on
- [Example 10 — DIMACS-9 routing](../10_dimacs9_routing) — shortest-path routing at dataset scale
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
