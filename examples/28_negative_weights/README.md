# Example 28 — Negative-weight routing

## What it demonstrates

Single-source shortest paths over a graph with **negative edge weights**,
where Dijkstra cannot go. It shows the three relevant `search` APIs working
together:

- `search.Dijkstra` **refuses** the problem — it returns
  `search.ErrNegativeWeight` (without traversing) the instant it sees a
  negative edge.
- `search.BellmanFord` **solves** it — it accepts signed weights and, on the
  acyclic instance, returns correct distances; on the `-arbitrage` instance
  it detects the negative cycle and returns `search.ErrNegativeCycle`.
- `search.JohnsonAPSP` **certifies** it — its reweighted all-pairs distances
  are used as an independent oracle for the Bellman-Ford answer.

The headline correctness fact is `bellman_ford_matches_johnson=true`: the
library's Bellman-Ford distances equal Johnson's reweighted row (and a
textbook reference) for every node.

## Domain / scenario

A **backhaul freight network**. Cargo leaves a single origin depot and
flows downstream through tiers of transshipment hubs to a final tier of
destination markets. Most lanes charge a positive shipping cost; some are
**backhaul lanes** where a carrier that would otherwise run empty pays a
**rebate** to fill the return leg — a lane with negative net cost. Routing
to a market therefore wants to chain rebate lanes, which is exactly a
negative-weight shortest-path problem.

The generator is a **seeded layered DAG**: every lane advances exactly one
tier (depot → hub → … → market), never backward. A directed acyclic graph
has no directed cycle at all, so it has **no negative cycle** regardless of
how negative the rebate lanes are — acyclicity is a genuine domain invariant
(goods flow one way), not a numerical trick. Fixing `-seed` fixes the lanes,
their weights, and therefore every shortest path exactly.

`-arbitrage` injects one back-edge (a market-tier hub back to the depot's
first hub) whose weight makes the two-node loop strictly negative for **any**
base weight. That models an "arbitrage loop" — a cycle you could traverse
forever being paid net, i.e. free money — which is physically impossible and
must be flagged. Bellman-Ford then reports `search.ErrNegativeCycle`.

## How to run

```sh
go run ./examples/28_negative_weights                        # small deterministic default
go run ./examples/28_negative_weights -layers 12 -width 4000  # observable-scale run
go run ./examples/28_negative_weights -arbitrage              # inject a negative cycle
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-layers` | number of tiers, including the single-node depot tier | `6` | `12` |
| `-width` | hubs/markets per non-depot tier | `16` | `4000` |
| `-fanout` | forward lanes per hub into the next tier | `4` | `8` |
| `-rebate-frac` | fraction of lanes that are rebate (negative) lanes | `0.30` | `0.30` |
| `-max-cost` | positive lane cost is drawn from `[1, max-cost]` | `100` | `100` |
| `-max-rebate` | rebate lane weight is drawn from `[-max-rebate, -1]` | `40` | `40` |
| `-arbitrage` | inject a negative cycle and assert it is detected | `false` | `true` |
| `-seed` | RNG seed; fixes the whole data shape | `1` | any |

Node count is `1 + (layers-1) * width`. Every depot→market path is exactly
`layers-1` hops, since each lane advances one tier.

## Expected output

At the default config (seed `1`) the **deterministic facts** are:

```
config.layers=6
config.width=16
config.fanout=4
config.rebate_frac=0.3
config.max_cost=100
config.max_rebate=40
config.arbitrage=false
config.seed=1
graph.nodes=81
graph.edges=252
graph.src=0
neg_edges=91
neg_cycle_detected=0
dijkstra_rejects_negative=true
bellman_ford_matches_johnson=true
market.0=44
market.1=-76
... (one line per market, market.0 .. market.15)
markets.count=16
markets.dist_sum=-1532
markets.dist_min=-175
markets.dist_max=44
cheapest_market=68
cheapest_cost=-175
cheapest_hops=5
cheapest_path=0>7>23>47>63>68
```

The example also prints **telemetry** lines prefixed with `#`, which vary
per run and per machine and are never asserted:

```
# build.elapsed=193µs
# mem.heap_alloc=277.28 KiB
# bf.latency=16µs
# bf.mallocs=15
# bf.relaxation_passes=5
# bf.edge_relaxations=192
# johnson.latency=200µs
```

Note the numbers that carry the message: 91 of the 252 lanes are rebate
(negative) lanes, and the cheapest route to a market costs **-175** — a
net-negative trip that Dijkstra could never have produced, which is why
`dijkstra_rejects_negative=true`. With `-arbitrage` the facts change to
`neg_cycle_detected=1` with `bellman_ford_detects`, `johnson_detects`, and
`reference_detects` all `true`, and no distance facts are emitted.

## Evidence it collects

This is a **search / path-finding** example, so it reports the dimensions
the standard's taxonomy lists for that subject, plus the negative-weight
specifics:

- **Correctness under negative weights** — the three-way oracle
  (`bellman_ford_matches_johnson`, cross-checked against a textbook
  reference) proves the Bellman-Ford distances are exactly right, and
  `dijkstra_rejects_negative` demonstrates the contract boundary between the
  two algorithms.
- **Negative-cycle detection** — the `-arbitrage` instance shows
  Bellman-Ford, Johnson, and the reference all detect a reachable negative
  cycle.
- **Wall-clock latency per query** — `# bf.latency`, `# johnson.latency`.
  Scale up and watch Johnson (all-pairs, `O(V·(V+E)·log V)`) grow far faster
  than the single-source Bellman-Ford, which is why Johnson is used only as
  an oracle.
- **Relaxation effort** — `# bf.relaxation_passes` (bounded by the tier
  depth for a layered DAG) and `# bf.edge_relaxations`.
- **Allocations and live heap** — `# bf.mallocs` (Bellman-Ford reaches a
  small constant allocation count after warm-up), `# mem.heap_alloc`,
  `# mem.bytes_per_edge`.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` / `AdjList.Compact` — build the
  mutable weighted directed freight graph (weights are `int64`, so lanes can
  be negative).
- `graph/csr.BuildFromAdjList` / `CSR.LiveNodes` / `CSR.NeighboursByID` —
  freeze into an immutable CSR snapshot and enumerate it for the reference
  solver and oracle.
- `search.BellmanFordCtx` / `search.Distances.Distance` /
  `search.Distances.Path` — single-source shortest paths over signed
  weights, distance read-back, and route reconstruction.
- `search.JohnsonAPSP` / `search.APSP.At` — all-pairs reweighting that
  tolerates negative edges, used as the correctness oracle.
- `search.DijkstraCtx` / `search.ErrNegativeWeight` — the non-negative
  algorithm and the sentinel it returns on a negative edge.
- `search.ErrNegativeCycle` — the sentinel Bellman-Ford and Johnson return
  on a reachable negative cycle.
- `graph.Mapper.Lookup` / `graph.Mapper.Resolve` — translate between stable
  domain indices and the hash-scattered graph NodeIDs.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [Example 01 — basic shortest paths](../01_basic) — the minimal Dijkstra flow
- [Example 14 — routing alternatives](../14_routing_alternatives) — Dijkstra, Yen, and A\* on one graph
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
