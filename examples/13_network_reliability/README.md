# Example 13 — Network reliability

## What it demonstrates

Two complementary resilience analyses run over **one coherent network**
derived from a single capacitated edge list: the structural single
points of failure (articulation points and bridges) found with
`search.HopcroftTarjanBCC` over an immutable CSR snapshot, and the
maximum source-to-sink throughput plus its limiting bottleneck (the
minimum cut), with the max flow computed by `search/flow`. The flow
network is built from the very same links the structural analysis sees,
so the two views describe a single network rather than two unrelated
graphs.

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
flow.max_value=20
flow.min_cut_size=2
flow.min_cut_capacity=20
flow.maxflow_eq_mincut=true
```

Interleaved with the facts are `# `-prefixed **telemetry** lines that vary
per run and per machine, for example:

```
# build.elapsed=81µs
# mem.heap_alloc=297.20 KiB
# spof.elapsed=12µs
# spof.articulation_point=c0s4
# spof.bridge=c0s4--stub4
# flow.elapsed=8µs
# flow.saturated_link=c2s1--c3s1 (10 Gb/s)
```

A regression test pins the fact lines and ignores every `# ` line.

## Evidence it collects

- **Structural analysis wall-clock** (`# spof.elapsed`) — the cost of
  Hopcroft-Tarjan biconnected components over the CSR snapshot, O(V + E).
- **Max-flow wall-clock** (`# flow.elapsed`) — the cost of Dinic's
  max-flow to settle the throughput.
- **Live heap** (`# mem.heap_alloc`, `# mem.heap_growth`) — the resident
  footprint of the snapshot and the flow network after a forced GC.
- **Build throughput** (`# build.site_rate`, `# build.link_rate`).

Scale it up with `-clusters` / `-cluster-size` and watch the SPOF and
flow wall-clocks grow with V and E while the deterministic facts (two
articulation points, one bridge, a two-link 20 Gb/s min-cut) stay fixed:
the topology's reliability structure is invariant under scale, only the
dense cores widen.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected backbone from the single link list.
- `graph.Mapper` — intern site names into compact `NodeID`s for SPOF resolution.
- `graph/csr.BuildFromAdjList` — freeze the backbone into an immutable CSR snapshot for the structural analysis.
- `search.HopcroftTarjanBCCCtx` — locate articulation points and bridges (single points of failure) in O(V + E), context-aware.
- `search/flow.NewNetwork` / `Network.AddEdge` / `flow.MaxFlowCtx` — Dinic's max-flow, used as the authoritative oracle cross-checked against the example's in-line residual solver, which also exposes the residual graph used to derive the minimum cut.

## Further reading

- [`search`](../../search) — traversal, connectivity, and path-finding package documentation
- [`search/flow`](../../search/flow) — max-flow and min-cut algorithms
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used by the structural analysis
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
