# Example 09 — Leiden community detection

## What it demonstrates

Modularity-optimising community detection with `community.Leiden`: build
an undirected graph, freeze it into an immutable CSR snapshot, run
Leiden, and read the resulting `Partition` back — including how to
handle the ghost-slot sentinel (`-1`) that sharded packing leaves in the
NodeID-indexed `Community` slice on small graphs.

## Domain / scenario

A textbook two-community graph: two `K4` cliques (nodes 0–3 and 4–7),
each a complete subgraph, joined by a single bridge edge `3–4`.

```
0───1        4───5
│╲ ╱│        │╲ ╱│
│ ╳ │        │ ╳ │
│╱ ╲│        │╱ ╲│
2───3────────4   (bridge 3–4)   6───7
```

Each clique is densely connected internally and the two halves touch
only through the one bridge, so a modularity-optimising method recovers
exactly the two cliques as separate communities.

## How to run

```sh
go run ./examples/09_leiden
```

## Expected output

```
Found 2 communities across 8 live nodes
  node 0 -> community 0
  node 1 -> community 0
  node 2 -> community 0
  node 3 -> community 0
  node 4 -> community 1
  node 5 -> community 1
  node 6 -> community 1
  node 7 -> community 1
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected graph (two cliques plus a bridge).
- `graph/adjlist.AdjList.Mapper` — translate the user node values (`int`) into compact `NodeID`s via `Lookup`.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `search/community.Leiden` / `DefaultLeidenOptions` — run Leiden community detection with the default parameters.
- `search/community.Partition` — the result: `NumCommunities` and a NodeID-indexed `Community` slice whose ghost slots carry the sentinel `-1`.

## Further reading

- [`search/community`](../../search/community) — community-detection package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 08 — PageRank](../08_pagerank) — another analytics algorithm over a CSR snapshot
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
