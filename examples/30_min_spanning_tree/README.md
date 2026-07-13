# Example 30 — Minimum spanning tree (least-cost backbone)

## What it demonstrates

GoGraph's two minimum-spanning-tree algorithms, `search.PrimMST` and
`search.KruskalMST`, run over one shared immutable CSR snapshot and
cross-checked against each other as a correctness oracle. It shows how to
build an undirected weighted graph, freeze it into a CSR, compute the MST two
independent ways, and prove they agree.

## Domain / scenario

A telecom/utility planner must interconnect a set of physical sites
(exchanges, cabinets, substations) with the cheapest possible cable run. Each
site has a 2-D geographic position; a candidate link's cost is its Euclidean
distance in metres. The planner surveyed more routes than they will build, so
the optimal backbone is the minimum-weight subset of links that keeps every
site connected — a minimum spanning tree (one connected region) or a minimum
spanning forest (several regions left disjoint).

The seeded generator lays out `regions` metro areas separated horizontally on
the plane. Within a region it first wires a random spanning tree (so the region
is always connected), then adds `extra-edges` redundant candidate links per
site, so the graph has strictly more candidate links than a tree and the MST
algorithms must genuinely *select* the cheapest connecting subset. With
`-interconnect` (the default) consecutive regions are chained by one long
inter-region link into a single connected backbone; with `-interconnect=false`
the regions stay disjoint and the result is a spanning forest of `regions`
trees.

## How to run

```sh
go run ./examples/30_min_spanning_tree                       # small deterministic default
go run ./examples/30_min_spanning_tree -interconnect=false   # spanning forest (one tree per region)
go run ./examples/30_min_spanning_tree -regions 4 -sites 200000 -extra-edges 3 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-regions` | number of geographic metro areas | `3` | `4` |
| `-sites` | sites placed in each region | `40` | `200000` |
| `-extra-edges` | redundant candidate links added per site | `2` | `3` |
| `-span` | region-local coordinate span, in metres | `10000` | `10000` |
| `-region-gap` | horizontal offset between region origins, in metres | `50000` | `50000` |
| `-interconnect` | chain consecutive regions into one backbone | `true` | `true` |
| `-seed` | RNG seed (fixes the network shape) | `1` | `7` |

The default builds 120 sites and runs both MST passes in microseconds, well
under the short-layer package budget. The large run is ~800 000 sites and
~3.2M candidate links, where Kruskal's edge sort dominates and its wall-clock
and allocation cost become measurable.

## Expected output

Deterministic *fact* lines at the default config (byte-stable for a fixed
seed); the `# `-prefixed *telemetry* lines vary per run and per machine:

```
config.regions=3
config.sites_per_region=40
config.extra_edges=2
config.interconnect=true
config.seed=1
sites.total=120
links.total=336
# build.elapsed=331µs
# mem.heap_alloc=218.51 KiB
# mem.heap_growth=14.86 KiB
graph.component_count=1
mst.total_weight=437920
mst.edge_count=119
mst.min_link_cost=249
mst.max_link_cost=53647
# kruskal.elapsed=1.645ms
# kruskal.mallocs=14
# prim.elapsed=967µs
# prim.mallocs=25
# mst.savings_pct=76.5
```

With `-interconnect=false` the facts become `graph.component_count=3`,
`mst.edge_count=117` (120 sites − 3 components), and `mst.total_weight=339175`.

## Evidence it collects

Following the search/path-finding row of the examples taxonomy:

- **Correctness (oracle).** `mst.total_weight` and `mst.edge_count` are the
  same whether computed by Prim or Kruskal; `run` returns a `MODULE BUG` error
  if the two disagree on total weight, on the sorted multiset of edge weights,
  on the unique-MST edge set (when all costs are distinct), or on the
  spanning-forest shape (`V − K` edges for `V` live sites in `K` components,
  and `V_c − 1` per component).
- **Per-algorithm cost.** `# kruskal.elapsed` / `# prim.elapsed` wall-clock and
  `# *.mallocs` transient allocations. Scale up `-sites` to watch Kruskal's
  `O(E log E)` sort overtake Prim's heap-driven `O(E log V)` expansion.
- **Backbone economics.** `# mst.savings_pct` is the cost saving of the optimal
  backbone versus laying every surveyed candidate link.

## Key APIs

- `search.PrimMST` / `search.PrimMSTCtx` — Prim's MST rooted at a source; used
  here once per connected component to build a spanning forest.
- `search.KruskalMST` / `search.KruskalMSTCtx` — Kruskal's whole-graph minimum
  spanning forest, returning `search.MSTEdge` records.
- `search.WCC` — weakly-connected components, for an independent component count
  and per-component Prim roots.
- `graph/adjlist.New` with `Config{Directed: false}` and `graph/csr.BuildFromAdjList`
  — build an undirected weighted graph and freeze it into a symmetric CSR.

## Further reading

- [`search`](../../search) — package documentation (`prim.go`, `kruskal.go`)
- [Example 03 — Advanced algorithms](../03_advanced_algorithms) — the shared-CSR
  idiom this example follows (build once, query many)
- [Example 13 — Network reliability](../13_network_reliability) — another
  weighted-graph analysis over a generated network
- [docs/examples-standard.md](../../docs/examples-standard.md) — the five-point
  rubric every example follows
