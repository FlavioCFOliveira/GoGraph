# Example 29 — All-pairs shortest paths

## What it demonstrates

Computing all-pairs shortest paths (APSP) over one shared, immutable CSR
snapshot with all three APSP algorithms GoGraph ships —
`search.DijkstraAPSP`, `search.FloydWarshall`, and `search.JohnsonAPSP` —
cross-checking that the three distance matrices are **bit-identical**, and
deriving the classical graph metrics (radius, diameter, per-node
eccentricity) from the result.

## Domain / scenario

A regional road network. `nodes` towns are scattered by a seeded RNG across
an elongated rectangular region (`width` × `height`). Two families of road
segment connect them:

- A **Euclidean minimum spanning tree** over the complete point set. The MST
  touches every town, so it alone guarantees the network is **connected for
  any seed** — every pair of towns therefore has a finite shortest-path
  distance and radius / diameter / eccentricity are well defined.
- Each town's **`knn` nearest neighbours**, added as extra segments. Real road
  networks are not trees; the k-nearest-neighbour edges add the redundant
  short links that give the network realistic detours and a smooth
  eccentricity gradient.

Every segment is undirected and carries a strictly positive `int64` weight —
the Euclidean length rounded to the nearest unit, floored at 1. The elongated
region stretches the network along one axis, so eccentricity varies smoothly
from a distinct central cluster of towns (low eccentricity — the radius) out
to the towns at the two far ends (high eccentricity — the diameter). The
topology was chosen on the advice of the `graph-theory-expert` sub-agent: a
geometric proximity graph yields a high-cardinality eccentricity gradient with
a genuine centre and periphery, where a hub-and-spoke network would collapse
the diameter toward the degenerate ratio `D = 2r`.

Because every edge weight is a positive integer the shortest-path distances are
exact and unique, so the three algorithms — which reach the answer by three
completely different routes (Floyd-Warshall's dense O(V³) DP, Dijkstra-from-
every-source, and Johnson's reweight-then-Dijkstra) — must produce identical
matrices. Any divergence would be a module bug.

## How to run

```sh
go run ./examples/29_all_pairs                                          # small deterministic default
go run ./examples/29_all_pairs -nodes 800 -knn 5 -width 1440 -height 480 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-nodes` | number of towns (the APSP dimension V) | `200` | `800` |
| `-knn` | nearest-neighbour road segments added per town | `4` | `5` |
| `-width` | width of the region (the long axis) | `360` | `1440` |
| `-height` | height of the region (the short axis) | `120` | `480` |
| `-seed` | RNG seed (fixes the town placement and every fact) | `1` | `7` |

The default builds a 200-town network in a 360×120 (3:1) region.
Floyd-Warshall's O(V³) is then ~8 × 10⁶ cell-updates — microseconds — well
under the 60 s short-test budget, yet the eccentricity gradient is already
rich. At `-nodes 800` Floyd-Warshall does ~5 × 10⁸ cell-updates against
Johnson's 800 sparse Dijkstra runs, so the wall-clock gap between the dense and
sparse approaches becomes stark.

## Expected output

The bare `key=value` lines are deterministic *facts*: reproducible for a fixed
(`-nodes`, `-knn`, `-width`, `-height`, `-seed`) on every run and machine, and
pinned by the regression test. The `# `-prefixed lines are volatile *telemetry*
(matrix footprint, per-algorithm wall-clock, heap): they vary per run and per
machine and are never pinned.

```
config.nodes=200
config.knn=4
config.region=360x120
config.seed=1
graph.nodes=200
graph.segments=493
apsp_three_way_agree=1
floyd_parallel_agree=1
johnson_parallel_agree=1
metric.radius=227
metric.diameter=429
metric.center_town=198
metric.periphery_town=17
metric.sample_town=0
metric.sample_eccentricity=287
metric.distinct_eccentricities=118
# matrix.result_footprint=351.56 KiB
# build.elapsed=5.147ms
# apsp.dijkstra.elapsed=3.840ms
# apsp.floyd_warshall.elapsed=5.032ms
# apsp.johnson.elapsed=2.640ms
# mem.heap_alloc=393.31 KiB
# mem.heap_growth=179.41 KiB
```

(The `# ` lines above are illustrative; expect different numbers on your
machine. The fact lines are stable.)

## Evidence it collects

- **Correctness (three-way oracle).** `apsp_three_way_agree=1` asserts that
  Dijkstra-APSP, Floyd-Warshall, and Johnson produced bit-identical distance
  matrices — a strong correctness cross-check on integer weights, where the
  distances are exact and unique. `floyd_parallel_agree` and
  `johnson_parallel_agree` extend the check to each parallel variant against
  its serial counterpart.
- **Graph metrics.** The radius, diameter, the centre town (an eccentricity
  minimiser), a peripheral town (an eccentricity maximiser), the sample town's
  eccentricity, and the count of distinct eccentricity values — evidence that
  the gradient is rich rather than degenerate (`r <= D <= 2r`, `D/r` around
  1.9 by construction).
- **Resource efficiency (telemetry).** The O(V²) dense distance-matrix
  footprint in bytes — the result size intrinsic to all-pairs, independent of
  how sparse the road graph is — and the per-algorithm wall-clock. The
  wall-clock lines are the evidence to watch when scaling up: Floyd-Warshall
  spends O(V³) filling the dense matrix while Dijkstra and Johnson exploit the
  road graph's sparsity in O(V·(V+E)·log V), so their advantage widens with V.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the undirected, weighted road network.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot; `CSR.Order` / `CSR.LiveNodes` report the live node set.
- `search.DijkstraAPSP` — APSP by one Dijkstra per source (non-negative weights); O(V·(V+E)·log V).
- `search.FloydWarshall` — the textbook O(V³) dynamic program over a dense matrix.
- `search.JohnsonAPSP` — Bellman-Ford reweighting then one Dijkstra per source; O(V·(V+E)·log V).
- `search.FloydWarshallParallel` / `search.JohnsonAPSPParallel` — parallel variants, bit-identical to their serial counterparts.
- `search.APSP.At` / `search.APSP.N` — read a pairwise distance (with a reachability bool) and the matrix dimension.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [Example 03 — advanced algorithms](../03_advanced_algorithms) — four algorithms over one CSR snapshot, with per-algorithm evidence
- [Example 10 — DIMACS 9 routing](../10_dimacs9_routing) — single-source shortest paths and a latency distribution
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
