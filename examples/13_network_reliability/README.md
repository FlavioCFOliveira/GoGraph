# Example 13 — Network reliability

## What it demonstrates

Two complementary resilience analyses run over **one coherent network**:
the structural single points of failure (articulation points and
bridges) found with `search.HopcroftTarjanBCC`, and the maximum
source-to-sink throughput plus its limiting bottleneck (the minimum
cut) computed with `search/flow`. The flow network is derived from the
very same capacitated edge list the structural analysis sees, so the
two views describe a single network rather than two unrelated graphs.

## Domain / scenario

A communication backbone of seven sites. The four core sites form a
redundant ring — `lisbon — madrid — paris — frankfurt` with a
`madrid — paris` cross-link — that no single failure can partition.
Three spur sites (`london`, `berlin`, `warsaw`) hang off single links.
Each link carries a capacity in Gb/s:

```
lisbon    -- madrid    (12)
lisbon    -- paris     ( 8)
madrid    -- paris     ( 6)
madrid    -- frankfurt (10)
paris     -- frankfurt ( 7)
paris     -- london    ( 5)
frankfurt -- berlin    ( 9)
berlin    -- warsaw    ( 4)
```

The redundancy makes both analyses non-trivial. Structurally, the ring
core is biconnected (no articulation point, no bridge), while the spurs
expose `paris`, `frankfurt`, and `berlin` as articulation points and
their single links as bridges. For throughput, the maximum flow from
`lisbon` to `frankfurt` splits across several paths through the ring,
so the bottleneck is not a single link but the **set** of links the
minimum cut severs.

The interesting result is the bottleneck: `lisbon` can push 17 Gb/s to
`frankfurt`, and the limit comes entirely from `frankfurt`'s two
incoming core links — `madrid — frankfurt` (10) and `paris — frankfurt`
(7) — which together sum to exactly 17. The max-flow min-cut theorem
guarantees that the saturated-link capacity equals the maximum flow,
and the example verifies this equality at runtime.

## How to run

```sh
go run ./examples/13_network_reliability
```

## Expected output

```
Single points of failure:
  articulation point: frankfurt
  articulation point: berlin
  articulation point: paris
  bridge: paris -- london
  bridge: berlin -- warsaw
  bridge: frankfurt -- berlin

Max throughput lisbon -> frankfurt: 17 Gb/s
Bottleneck (min-cut, 17 Gb/s) — saturated links:
  madrid -- frankfurt (10 Gb/s, fully utilised)
  paris -- frankfurt (7 Gb/s, fully utilised)
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable undirected backbone from one capacitated edge list.
- `graph/adjlist.AdjList.Mapper` — intern site names into compact `NodeID`s; the same `NodeID` space backs the flow network.
- `graph/csr.BuildFromAdjList` — freeze the backbone into an immutable CSR snapshot for the structural analysis.
- `search.HopcroftTarjanBCC` — locate articulation points and bridges (single points of failure) in O(V + E).
- `search/flow.NewNetwork` / `Network.AddEdge` / `flow.MaxFlow` — Dinic's max-flow, cross-checked against the example's in-line Edmonds-Karp solver, which also exposes the residual graph used to derive the minimum cut.

## Further reading

- [`search`](../../search) — traversal, connectivity, and path-finding package documentation
- [`search/flow`](../../search/flow) — max-flow and min-cut algorithms
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used by the structural analysis
- [Example 01 — basic shortest paths](../01_basic) — the minimal build-and-query flow this example builds on
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
