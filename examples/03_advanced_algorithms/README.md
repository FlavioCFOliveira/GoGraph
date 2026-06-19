# Example 03 — Advanced algorithms

## What it demonstrates

Running four GoGraph algorithms over a single immutable CSR snapshot:
breadth-first traversal (`search.BFS`), single-source shortest paths
(`search.Dijkstra`), exact betweenness centrality (`centrality.Betweenness`,
Brandes' algorithm), and PageRank (`centrality.PageRank`). One snapshot is
built once and queried by every algorithm, and the example reports
per-algorithm evidence — wall-clock, PageRank convergence iterations,
transient allocations, and live heap.

## Domain / scenario

A seeded synthetic network chosen so all four algorithms produce meaningful,
dramatically non-uniform results from one graph. The generator builds `C`
**Barabási–Albert scale-free communities** (preferential attachment, so a few
high-degree hubs dominate each community) and joins them **only through
dedicated low-degree bridge nodes wired in a ring**. The graph is undirected;
edge weights are positive with spread.

The topology was chosen on the advice of the `graph-theory-expert` sub-agent,
and it makes two centralities disagree on purpose:

- The **bridge nodes are cut vertices** — the sole gateway out of each
  community — so every inter-community shortest path is forced through one of
  them. Their **betweenness** towers over every other node.
- The **Barabási–Albert hubs** dominate **PageRank** (the stationary
  random-surfer mass), while the bridges, being low-degree, rank low.

So the same nodes have *high betweenness and low PageRank* — the cut structure
governs betweenness, the attach-point degree governs PageRank. Only Dijkstra
consumes the edge weights; BFS counts hops, and both Brandes betweenness and
PageRank here are the unweighted variants, so Dijkstra's weighted distance
generally differs from the BFS hop count.

## How to run

```sh
go run ./examples/03_advanced_algorithms                                      # small deterministic default
go run ./examples/03_advanced_algorithms -communities 8 -nodes 500 -ba-attach 3 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-communities` | number of scale-free communities (also the number of bridge nodes / ring size) | `4` | `8` |
| `-nodes` | Barabási–Albert nodes per community | `25` | `500` |
| `-ba-attach` | BA attachment parameter `m` (edges each new node brings; must be `< -nodes`) | `2` | `3` |
| `-bridge-intra-edges` | edges from each bridge node into its community (kept low so the bridge stays a clean cut vertex) | `1` | `2` |
| `-weight-min` | minimum edge weight, inclusive (`>= 1`, so Dijkstra has positive weights) | `1` | `1` |
| `-weight-max` | maximum edge weight, inclusive | `10` | `100` |
| `-top-k` | how many top betweenness / PageRank nodes to report | `5` | `5` |
| `-seed` | RNG seed (fixes the data shape exactly) | `1` | `7` |

The default is `4*25 + 4 = 104` nodes and ~196 undirected edges, so the
`O(V*E)` Brandes pass runs in microseconds — fast and deterministic for the
regression test. The observable-scale run is ~4 000 nodes and ~12 000 edges,
where the per-algorithm cost (especially Brandes) becomes visible.

## Expected output

At the default config, the deterministic FACT lines are:

```
config.communities=4
config.nodes_per_community=25
config.ba_attach=2
config.weights=[1,10]
config.seed=1
nodes.total=104
edges.total=392
nodes.bridges=4
bfs.reachable=104
bfs.eccentricity=10
dijkstra.dist_to_farthest=33
dijkstra.hops_to_farthest=7
betweenness.top1=100
betweenness.top2=101
betweenness.top3=102
betweenness.top4=103
betweenness.top5=74
pagerank.iterations=34
pagerank.top1=2
pagerank.top2=76
pagerank.top3=51
pagerank.top4=52
pagerank.top5=29
```

Node ids `100`–`103` are the four bridge nodes: they are exactly the top-4
betweenness nodes (cut vertices) and never appear in the PageRank top-5. The
`edges.total` is `392` because the immutable CSR counts each undirected edge as
two directed entries. Interleaved with the facts, the example also prints
volatile telemetry lines, for example:

```
# betweenness.elapsed=964µs   # varies per run and per machine
```

Telemetry lines are prefixed with `# ` and vary per run and per machine; the
regression test pins only the bare fact lines.

## Evidence it collects

This is a centrality/traversal example, so it reports, per algorithm
(`# ` telemetry): wall-clock latency, PageRank convergence iterations, and the
transient allocations made during the algorithm (`runtime.MemStats.Mallocs`
delta), plus the live heap after building the snapshot. When you scale it up
with `-communities`/`-nodes`/`-ba-attach`, watch the Brandes wall-clock grow
with `O(V*E)` while BFS, Dijkstra, and PageRank stay comparatively cheap, and
confirm the deterministic facts still hold (the bridges still top betweenness,
the hubs still top PageRank).

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected weighted graph.
- `graph/csr.BuildFromAdjList` — freeze the builder into the immutable CSR snapshot shared by every algorithm.
- `graph/csr.CSR.LiveNodes` — enumerate NodeIDs with an incident edge, for the name-/value-resolved top-k report.
- `search.BFS` / `search.BFSCtx` — breadth-first traversal in non-decreasing depth order.
- `search.Dijkstra` / `search.DijkstraCtx` — single-source shortest paths over non-negative weights.
- `search/centrality.Betweenness` / `BetweennessCtx` — exact betweenness centrality via Brandes' algorithm.
- `search/centrality.PageRank` / `PageRankCtx` / `DefaultPageRankOptions` — stationary random-surfer importance scores.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`search/centrality`](../../search/centrality) — betweenness and PageRank package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the shared query surface
- [Example 01 — basic shortest paths](../01_basic) — the minimal Dijkstra flow this example builds on
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
