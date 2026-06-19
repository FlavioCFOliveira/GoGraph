# Example 16 — Centrality analytics

## What it demonstrates

Two analytics over one shared, immutable CSR snapshot: exact
**betweenness centrality** via Brandes' algorithm
(`search/centrality.BetweennessCtx`), which scores each node by how many
shortest paths run through it, and **label-propagation community
detection** (`search/community.LabelPropagationCtx`), which partitions the
graph into communities. It also shows how to make analytics output
deterministic in the face of structural ties — the betweenness ranking
breaks score ties by node id — and reports per-analysis evidence
(wall-clock, transient allocations, live heap).

## Domain / scenario

A seeded synthetic network shaped as a **chain of dense clusters joined by
single bridge edges**:

```
C0 == C1 == C2 == … == C(K-1)
```

Each cluster is a dense Erdős–Rényi subgraph laid down on top of a random
spanning tree (the tree guarantees the cluster is one connected component;
the extra edges at `-intra-density` make it dense). Consecutive clusters are
joined by exactly one bridge edge, between the right gateway of cluster `c`
and the left gateway of cluster `c+1`.

The topology serves both analytics on purpose:

- **Betweenness** concentrates on the gateways. Each bridge is the unique
  edge across its cut, so every shortest path between the two sides must
  traverse both gateway endpoints — the gateways carry Θ(n_c²)
  pair-dependencies while interior nodes carry only O(n_c). The gateways are
  therefore the betweenness winners for *every* seed: this is a theorem of
  the cut structure, not a heuristic. A chain (not a ring) is used so the
  inter-cluster shortest path is unique, keeping the winners unambiguous.
- **Label propagation** keeps each dense cluster as one label (the single
  bridge cannot out-vote a node's many intra-cluster neighbours) and the
  near-zero inter-cluster density is the stable regime that stops a label
  flooding across. It does, however, intermittently **merge** two whole
  adjacent clusters across a bridge — the documented "monster community"
  coarsening of synchronous label propagation (Raghavan–Albert–Kumara 2007;
  Leung et al. 2009), which the example reports honestly rather than tuning
  away.

## How to run

```sh
go run ./examples/16_centrality_analytics                                       # small deterministic default
go run ./examples/16_centrality_analytics -communities 20 -nodes 200 -seed 7    # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-communities` | number of clusters in the chain | `6` | `20` |
| `-nodes` | nodes per cluster (≥ 2: two are gateways) | `50` | `200` |
| `-intra-density` | Erdős–Rényi p_in for extra intra-cluster edges, in [0,1] | `0.30` | `0.20` |
| `-top-k` | how many top-betweenness node ids to report | `10` | `40` |
| `-seed` | RNG seed (fixes the data shape exactly) | `1` | any |

The default builds 300 nodes and ~1.5k edges. Exact Brandes is O(V·E), so
the default is deliberately small (it runs in milliseconds); the large run
pushes the betweenness pass into seconds where its cost is observable.

## Expected output

At the default config the deterministic **fact** lines are:

```
config.communities=6
config.nodes_per_community=50
config.intra_density=0.3
config.top_k=10
config.seed=1
nodes.total=300
edges.total=4836
nodes.gateways=10
betweenness.top1=101
betweenness.top2=150
betweenness.top3=151
betweenness.top4=100
betweenness.top5=200
betweenness.top6=51
betweenness.top7=201
betweenness.top8=50
betweenness.top9=250
betweenness.top10=1
communities.count=5
communities.sizes=[50 50 50 50 100]
```

The ten `betweenness.top*` ids are exactly the ten bridge gateways
`{1, 50, 51, 100, 101, 150, 151, 200, 201, 250}`; their *order* varies with
the seed (gateways tie on score), but the *set* does not. `communities.count`
is 5 here because label propagation merged two adjacent clusters into one
community of 100; at other seeds it is 6 with sizes `[50 50 50 50 50 50]`.

Interleaved with the facts are volatile **telemetry** lines, prefixed with
`# `, for example:

```
# betweenness.elapsed=8.979ms
# betweenness.mallocs=12
# communities.elapsed=544µs
# mem.heap_alloc=314.78 KiB
```

Telemetry varies per run and per machine; the regression test pins the fact
lines and ignores every `# ` line.

## Evidence it collects

For the centrality/community subject (per `docs/examples-standard.md`): the
**per-analysis wall-clock** (`# betweenness.elapsed`, `# communities.elapsed`),
**transient allocations** (`# *.mallocs`, the `runtime.MemStats.Mallocs`
delta around each pass), and **live heap** (`# mem.heap_alloc`,
`# mem.heap_growth`). Scale it up with `-communities` / `-nodes` and watch
the betweenness elapsed grow as O(V·E) while the allocation counts stay flat
(the algorithms reuse buffers), and watch `communities.count` fluctuate in
`[K/2, K]` as label propagation coarsens the chain.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected network.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `graph/adjlist.AdjList.Mapper` / `graph.Mapper.Resolve` — translate compact `NodeID`s back to user-facing node ids.
- `search/centrality.BetweennessCtx` — exact Brandes betweenness centrality, returned as a `NodeID`-indexed `[]float64`.
- `search/community.LabelPropagationCtx` / `DefaultLabelPropagationOptions` — community detection; `Partition.Community` is a `NodeID`-indexed slice of community IDs and `Partition.NumCommunities` counts the live communities.

## Further reading

- [`search/centrality`](../../search/centrality) — centrality metrics package documentation
- [`search/community`](../../search/community) — community-detection package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the analytics surface
- [Example 03 — Advanced algorithms](../03_advanced_algorithms) — four algorithms (incl. Brandes) over one snapshot on a related seeded topology
- [Example 08 — PageRank](../08_pagerank) — a related node-importance metric
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
