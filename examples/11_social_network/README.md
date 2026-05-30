# Example 11 — Social network analytics

## What it demonstrates

A small end-to-end social-network workload over a labelled property
graph: attach labels (`User`, `Verified`) and typed properties (`age`)
to people, freeze the graph into an immutable CSR snapshot, then run
three analytics over it — PageRank influence ranking, Leiden community
detection, and a manual two-hop friend-of-friend recommendation walk.

## Domain / scenario

Seven people form an undirected friendship graph. Each person is a
`User` node carrying an `age` property; the three accounts that are
verified also carry a `Verified` label. Friendships are unweighted,
symmetric edges:

```
alice — bob      bob   — dave
alice — carol    carol — erin
alice — erin     carol — frank
dave  — grace    erin  — grace
```

The analytics then read this graph three ways: who is most central
(PageRank), which clusters the friendships form (Leiden), and who
`alice` should befriend next (the people two hops from her who are not
already her friends).

## How to run

```sh
go run ./examples/11_social_network
```

## Expected output

```
Influence (PageRank):
  carol    0.1855
  alice    0.1786
  erin     0.1786
  dave     0.1294
  bob      0.1270
  grace    0.1270
  frank    0.0740

Communities (Leiden):
  community 0: [alice bob dave]
  community 1: [erin grace]
  community 2: [carol frank]

Friend-of-friend recommendations for alice:
  -> dave
  -> frank
  -> grace
```

The output is byte-stable: PageRank ties (`alice`/`erin` and
`bob`/`grace` share a score) are broken by name, the Leiden clusters
are printed in sorted cluster-id order, and the recommendations are
sorted by shared-friend count then name.

## Key APIs

- `graph/lpg.New` / `Graph.SetNodeLabel` / `Graph.SetNodeProperty` — build the labelled property graph and attach typed `age` values via `lpg.Int64Value`.
- `graph/lpg.Graph.AddEdge` — add the undirected friendship edges.
- `graph/csr.BuildFromAdjList` — freeze the live adjacency list into an immutable CSR snapshot for analytics.
- `search/centrality.PageRank` / `centrality.DefaultPageRankOptions` — rank users by influence.
- `search/community.Leiden` / `community.DefaultLeidenOptions` — detect communities; the result's `Community` slice maps each `NodeID` to a cluster id.
- `graph/adjlist.AdjList.Mapper` (`Resolve`) / `AdjList.Neighbours` — translate `NodeID`s back to names and walk the live adjacency list for the friend-of-friend recommendation.

## Further reading

- [`search/centrality`](../../search/centrality) — PageRank and other centrality measures
- [`search/community`](../../search/community) — Leiden and community-detection algorithms
- [`graph/lpg`](../../graph/lpg) — the labelled property-graph builder used here
- [Example 08 — PageRank](../08_pagerank) — PageRank in isolation
- [Example 09 — Leiden](../09_leiden) — Leiden community detection in isolation
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
