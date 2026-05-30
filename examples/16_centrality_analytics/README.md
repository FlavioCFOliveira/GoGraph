# Example 16 — Centrality analytics

## What it demonstrates

Two complementary graph-analytics metrics over an immutable CSR
snapshot: exact **betweenness centrality** via Brandes' algorithm
(`search/centrality.Betweenness`), which scores each node by how many
shortest paths run through it, and **label propagation**
(`search/community.LabelPropagation`), which partitions the graph into
communities. The example also shows how to make analytics output
deterministic in the face of structural ties.

## Domain / scenario

A small undirected social network of six people arranged as two
triangles joined by a single bridge:

```
Cluster 1            Cluster 2
  marie               jose
  /   \      bridge   /   \
pierre-anne  marie--jose  ana-luis
```

Edges: `marie--pierre`, `marie--anne`, `anne--pierre` (cluster 1);
`jose--ana`, `jose--luis`, `luis--ana` (cluster 2); and the single
`marie--jose` bridge that links them.

`marie` and `jose` are the two bridge endpoints, so every shortest path
that crosses between the clusters passes through both — they share the
highest betweenness score (12.00) and are structurally symmetric. The
remaining four nodes lie on no inter-cluster shortest path and score
0.00. Label propagation recovers the two original triangles as
communities.

## How to run

```sh
go run ./examples/16_centrality_analytics
```

## Expected output

```
Betweenness (higher = more critical):
  jose    12.00
  marie   12.00
  ana     0.00
  anne    0.00
  luis    0.00
  pierre  0.00

Label propagation clusters:
  community 0: [ana jose luis]
  community 1: [anne marie pierre]
```

The output is fully deterministic. Because `marie` and `jose` tie at
12.00 (and four nodes tie at 0.00), the ranking breaks score ties by
node name; the cluster listing sorts both the community IDs and the
member names. Without those tie-breakers the order would depend on
`sort.Slice` instability and Go map-iteration order.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected network.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `graph/adjlist.AdjList.Mapper` / `graph.Mapper.Resolve` — translate compact `NodeID`s back to person names.
- `search/centrality.Betweenness` — exact Brandes betweenness centrality, returned as a `NodeID`-indexed `[]float64`.
- `search/community.LabelPropagation` / `DefaultLabelPropagationOptions` — community detection; `Partition.Community` is a `NodeID`-indexed slice of community IDs.

## Further reading

- [`search/centrality`](../../search/centrality) — centrality metrics package documentation
- [`search/community`](../../search/community) — community-detection package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the analytics surface
- [Example 08 — PageRank](../08_pagerank) — a related node-importance metric
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
