# Example 01 — Basic shortest paths

## What it demonstrates

The minimal end-to-end GoGraph routing flow, scaled to a size where the
search work is observable: build a weighted directed graph with the mutable
`adjlist` builder, freeze it into an immutable CSR snapshot with
`csr.BuildFromAdjList`, run single-source `search.Dijkstra` over the whole
graph, and reconstruct a concrete multi-hop route from the result's parent
chain through the `graph.Mapper`.

## Domain / scenario

A directed transport network — a seeded **random geometric graph** that
models a road network. `-nodes` junctions are placed at seeded integer
coordinates in a `-span` × `-span` square; any two junctions within a
connection radius are joined by a road in both directions, weighted by the
integer-rounded straight-line distance between them (always ≥ 1). The radius
is tuned just above the geometric-graph connectivity threshold
(`r ≈ span·√(ln N / (π·N))`) so the network forms one connected component,
and an id-ordered backbone (junction `i ↔ i+1`, carrying its true geometric
weight) is laid down as a synthetic connectivity guarantee so every junction
is reachable from the source for any seed and scale. The backbone roads are
long, so Dijkstra routes around them through the short local roads — the
shortest paths are genuinely multi-hop, which is what makes this a
meaningful Dijkstra exercise rather than a one-edge lookup.

The query is anchored at junction `0`. The example reports the shortest
distance to three fixed target junctions (`nodes/4`, `nodes/2`, `nodes-1`)
and the full reconstructed route to the last of them.

## How to run

```sh
go run ./examples/01_basic                        # small deterministic default
go run ./examples/01_basic -nodes 1000000 -seed 7 # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-nodes` | Number of junctions to place | `5000` | `1000000` |
| `-span` | Side length of the coordinate square | `4000` | `200000` |
| `-radius` | Connection radius as a multiple of the auto-tuned connectivity threshold | `1.0` | `0.8` (sparser) … `2.0` (denser) |
| `-seed` | RNG seed; fixes the data shape exactly | `1` | any `int64` |

The default builds in tens of milliseconds and stays well under the 60 s
short-test budget; the data shape is reproducible for a fixed `-seed`. Scale
`-nodes` up (keeping `-span` proportional, e.g. `span ≈ 40·√nodes`, to hold
the road density roughly constant) to make the build and query costs
interesting.

## Expected output

The bare lines are deterministic **facts** (reproducible for the default
seed); the `# `-prefixed lines are volatile **telemetry** that varies per
run and per machine.

```
config.nodes=5000
config.span=4000
config.radius=139
config.seed=1
nodes.junctions=5000
edges.roads=101974
query.reachable=5000
dist.to_1250=811
dist.to_2500=3945
dist.to_4999=3388
route.to_4999.hops=6
route.to_4999=0 -> 3925 -> 3064 -> 3913 -> 3914 -> 336 -> 4999
```

A representative telemetry line (varies per run and per machine):

```
# query.dijkstra.elapsed=992µs
```

## Evidence it collects

This example's subject is search / path-finding, so it reports (from the
evidence taxonomy in [`docs/examples-standard.md`](../../docs/examples-standard.md)):

- **Build throughput** — junctions/s and roads/s while materialising the
  network (`# build.node_rate`, `# build.edge_rate`).
- **Snapshot freeze cost** — wall-clock to freeze the mutable builder into
  the immutable CSR query surface (`# freeze.elapsed`).
- **Single-source query latency** — wall-clock for the whole-graph Dijkstra
  run, and the implied settled-node rate (`# query.dijkstra.elapsed`,
  `# query.dijkstra.node_rate`).
- **Reachability** — the number of junctions Dijkstra settles
  (`query.reachable`, a deterministic fact).
- **Live heap** — `runtime.MemStats.HeapAlloc` after a forced GC, before and
  after the build (`# mem.heap_alloc`, `# mem.heap_growth`).

When you scale `-nodes` up, watch how the Dijkstra latency grows with the
edge count (it is `O((V+E)·log V)`), and how the live heap tracks the road
count.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable weighted directed graph.
- `graph/adjlist.AdjList.Mapper` — intern junction ids as compact `NodeID`s and walk them (`Lookup`, `Resolve`, `Walk`).
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable, lock-free CSR snapshot.
- `graph/csr.CSR.NeighboursByID` — iterate a junction's outgoing roads (used by the regression test to verify the route).
- `search.Dijkstra` — single-source shortest paths over non-negative weights.
- `search.Distances.Distance` / `search.Distances.Path` — read back the cost and reconstruct the route to each junction.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 02 — property graph](../02_property_graph) — the next example, building on this flow
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the scaled-evidence reference this example is modelled on
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
