# Example 13 — Network reliability

## What it demonstrates

A suite of resilience analyses over **one coherent network** derived from
a single capacitated edge list:

- **Structural single points of failure** — articulation points and
  bridges, found with `search.HopcroftTarjanBCC` over an immutable CSR
  snapshot.
- **Connectivity under failure** — the weakly-connected-component count
  (`search.WCC`) before and after the articulation bridge is removed: it
  rises from one to two, confirming the bridge is the stub's sole
  connector. `search.WCCParallel` reproduces the identical partition.
- **Throughput and its bottleneck** — the maximum source-to-sink flow
  (Dinic, `search/flow`) plus the minimum cut that limits it.
- **Max-flow algorithm cross-agreement** — `flow.EdmondsKarp` and
  `flow.PushRelabelMaxFlow` must return the same value as Dinic, an
  algorithm-agreement oracle.
- **Global min-cut and min-cost routing** — `flow.StoerWagner` finds the
  cheapest undirected cut anywhere in the backbone (the off-spine
  bridge, cheaper than the source-to-sink cut), and `flow.MinCostMaxFlow`
  solves a small deterministic per-link-cost routing scenario.

Every analysis but the last is built from the very same links the
structural analysis sees, so the views describe a single network rather
than several unrelated graphs.

## Domain / scenario

A communication backbone modelled as a deterministic, seeded
**transit-stub** clustered network — the GT-ITM transit-stub model from
the Internet-topology literature (Zegura, Calvert & Bhattacharjee, "How
to Model an Internetwork", IEEE INFOCOM '96). The generator produces, for
every seed, a network with genuine reliability structure:

- **`clusters` dense clusters** of `cluster-size` sites each. Each cluster
  is a Hamiltonian cycle plus `chords` random chords. The cycle alone
  makes a cluster 2-vertex-connected (removing any one site leaves a path
  on the rest), so a cluster has **no internal articulation point and no
  internal bridge**; chords keep it 2-connected and raise its internal
  capacity. Intra-cluster links carry the high capacity **H = 100 Gb/s**.
- **A spine path** (cluster 0 … K-1). Consecutive clusters are joined by a
  set of parallel inter-cluster links of capacity **M = 10 Gb/s** each:
  one interior boundary is deliberately the **narrowest** (two links);
  every other boundary has three. Because the cluster graph is a tree,
  every spine boundary is a genuine source-to-sink cut, so the narrowest
  one is the global bottleneck.
- **One off-spine stub cluster** joined to the spine by a **single** link
  of capacity **L = 5 Gb/s**. As the only path to the stub it is a
  **bridge**, and both its endpoints are **articulation points**. It sits
  off the source-sink spine, so it never enters the source-sink min-cut.
- The **source** is an interior site of cluster 0 and the **sink** an
  interior site of cluster K-1, each with high incident capacity. This
  defeats the trivial "isolate the source/sink" degree cut and forces the
  global min-cut to be the narrowest interior boundary — a **set of two
  saturated links**, strictly cheaper than either terminal's capacity and
  distinct from the bridge.

The result: for every seed and scale the network has exactly one bridge
with two articulation-point endpoints, and a non-trivial two-link min-cut.
The max-flow min-cut theorem (Ford & Fulkerson 1956) guarantees the
saturated-link capacity equals the maximum flow, and the example verifies
this equality at runtime.

## How to run

```sh
go run ./examples/13_network_reliability                                   # small deterministic default
go run ./examples/13_network_reliability -clusters 200 -cluster-size 64 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-clusters` | number of spine clusters (K); ≥ 2 for an interior boundary | `5` | `200` |
| `-cluster-size` | sites per cluster (s); must be > 3 (the widest boundary) | `8` | `64` |
| `-chords` | extra random chords per cluster beyond the Hamiltonian cycle | `8` | `64` |
| `-seed` | RNG seed; fixes the deterministic topology exactly | `1` | any |

The default builds 48 sites and runs in well under the 60 s short-test
budget. The large invocation builds ~12.9k sites / ~26k links, where the
structural analysis and max-flow wall-clock and the live-heap footprint
become observable; the deterministic facts are unchanged.

## Expected output

The deterministic **fact** lines at the default config:

```
config.clusters=5
config.cluster_size=8
config.chords=8
config.seed=1
nodes.sites=48
edges.links=108
spof.articulation_points=2
spof.bridges=1
wcc.components_connected=1
wcc.components_after_bridge_removal=2
wcc.parallel_matches_serial=true
flow.max_value=20
flow.min_cut_size=2
flow.min_cut_capacity=20
flow.maxflow_eq_mincut=true
maxflow.dinic=20
maxflow.edmondskarp=20
maxflow.pushrelabel=20
maxflow.algorithms_agree=true
stoerwagner.mincut_weight=5
stoerwagner.smaller_side_sites=8
mincostflow.flow=20
mincostflow.cost=136
```

Interleaved with the facts are `# `-prefixed **telemetry** lines that vary
per run and per machine, for example:

```
# build.elapsed=81µs
# mem.heap_alloc=297.20 KiB
# spof.elapsed=12µs
# spof.articulation_point=c0s4
# spof.bridge=c0s4--stub4
# wcc.serial_elapsed=8µs
# wcc.parallel_elapsed=10µs
# flow.elapsed=8µs
# flow.saturated_link=c2s1--c3s1 (10 Gb/s)
# maxflow.edmondskarp_elapsed=25µs
# maxflow.pushrelabel_elapsed=48µs
# stoerwagner.elapsed=319µs
# mincostflow.elapsed=2µs
```

At an observable-scale run (`-clusters 200 -cluster-size 64`, ~12.9k
sites) `flow.StoerWagner` is skipped — its O(V³) cost is impractical
there — and its two facts are replaced by a `# stoerwagner.skipped=…`
telemetry line; every other fact is unchanged.

A regression test pins the fact lines and ignores every `# ` line.

## Evidence it collects

- **Structural analysis wall-clock** (`# spof.elapsed`) — the cost of
  Hopcroft-Tarjan biconnected components over the CSR snapshot, O(V + E).
- **Connectivity wall-clock** (`# wcc.serial_elapsed`,
  `# wcc.parallel_elapsed`) — serial versus parallel weakly-connected
  components over the severed snapshot.
- **Per-algorithm max-flow wall-clock** (`# flow.elapsed`,
  `# maxflow.edmondskarp_elapsed`, `# maxflow.pushrelabel_elapsed`) — the
  three max-flow algorithms settling the same throughput, side by side.
- **Global min-cut wall-clock** (`# stoerwagner.elapsed`) — the O(V³)
  Stoer-Wagner cut over the whole backbone.
- **Live heap** (`# mem.heap_alloc`, `# mem.heap_growth`) — the resident
  footprint of the snapshot and the flow network after a forced GC.
- **Build throughput** (`# build.site_rate`, `# build.link_rate`).

Scale it up with `-clusters` / `-cluster-size` and watch the SPOF, WCC,
and flow wall-clocks grow with V and E while the deterministic facts (two
articulation points, one bridge, the 1 → 2 component split, a two-link
20 Gb/s min-cut, and the three algorithms agreeing) stay fixed: the
topology's reliability structure is invariant under scale, only the
dense cores widen.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected backbone from the single link list.
- `graph.Mapper` — intern site names into compact `NodeID`s for SPOF resolution.
- `graph/csr.BuildFromAdjList` — freeze the backbone into an immutable CSR snapshot for the structural analysis.
- `search.HopcroftTarjanBCCCtx` — locate articulation points and bridges (single points of failure) in O(V + E), context-aware.
- `search.WCCCtx` / `search.WCCParallelCtx` — weakly-connected-component count before and after the bridge is severed; the parallel variant must reproduce the serial partition.
- `search/flow.NewNetwork` / `Network.AddEdge` / `flow.MaxFlowCtx` — Dinic's max-flow, used as the authoritative oracle cross-checked against the example's in-line residual solver, which also exposes the residual graph used to derive the minimum cut.
- `search/flow.EdmondsKarpCtx` / `search/flow.PushRelabelMaxFlowCtx` — two further max-flow algorithms cross-checked against Dinic's value.
- `search/flow.StoerWagnerCtx` — global (undirected) minimum cut over the whole backbone.
- `search/flow.NewCostNetwork` / `CostNetwork.AddCostEdge` / `flow.MinCostMaxFlowCtx` — minimum-cost maximum flow for a small per-link-cost routing scenario.

## Further reading

- [`search`](../../search) — traversal, connectivity, and path-finding package documentation
- [`search/flow`](../../search/flow) — max-flow and min-cut algorithms
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used by the structural analysis
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
