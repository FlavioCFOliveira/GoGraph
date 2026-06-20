# Example 26 — Social-network scale benchmark

## What it demonstrates

Building a large labelled property graph that models a social network and
measuring **query performance** and **resource consumption** over it: the
example reports build throughput, Go heap footprint, and the latency of a
battery of representative Cypher queries (label-scan counts, relationship
counts, a friend-of-friend traversal, and a trending-articles
aggregation).

## Domain / scenario

A social network of `USER` and `ARTICLE` nodes:

```
(:USER    {id, name})                  // id is a 24-char hex string, name is realistic
(:ARTICLE {id, title})                 // id is a 24-char hex string, title is realistic
(:USER)-[:FRIEND {since}]->(:USER)     // friends-min .. friends-max per user
(:USER)-[:LIKE   {when}]->(:ARTICLE)   // 0 .. likes-max per user
```

`FRIEND` is a directed out-edge: each user is given a random out-degree in
`[friends-min, friends-max]` to distinct other users (no self-loops, no
duplicate targets). `LIKE` is a directed out-edge to between zero and
`likes-max` distinct articles. The dataset is generated from a seeded RNG,
so its shape is reproducible for a fixed `-seed`; only the telemetry varies
between runs.

Every relationship carries exactly one **mandatory date** property:
`FRIEND.since` records when the friendship was created and `LIKE.when`
records when the like happened, both always present. They are stored as
ISO-8601 (`YYYY-MM-DD`) strings — the representation example 25 also uses
for Cypher-queryable timestamps — so the engine reads them back as
non-null values, and because ISO-8601 sorts chronologically, range and
`ORDER BY` predicates over `since`/`when` behave as dates. (`lpg.TimeValue`
is deliberately **not** used here: the Cypher reader maps it to null,
whereas the date strings round-trip.) The dates are drawn from the seeded
RNG anchored to a fixed reference date — never the wall clock — so they are
reproducible for a fixed `-seed`. The query battery includes two coverage
queries that count relationships whose date `IS NOT NULL`; they always
equal the total relationship counts, which is the always-filled invariant
the regression test asserts.

The graph is built in memory and queried with an in-memory
`cypher.Engine`. The example deliberately does **not** exercise the
WAL/recovery stack: durably persisting hundreds of millions of edges is
impractical for an example and orthogonal to what this one measures
(persistence is covered by examples 04, 17, 24 and 25).

## How to run

```sh
go run ./examples/26_social_scale_bench
```

With **no flags** the example builds the full specification: **1,000,000
users**, **30,000 articles**, **150–200 friends per user**, and **up to
300 likes per user** — roughly 1.03M nodes and ~3.2 × 10⁸ edges.

> **Resource warning.** Each edge carries a mandatory date property,
> and at this model's degrees that per-edge property store dominates
> resident heap: **~159 bytes of live heap per edge** (measured at 20k/2k,
> explicit types — see below). The full run therefore needs on the order of
> **~48 GiB of live heap** and several minutes to build. The implicit-type
> mode (`-rel-types=false`) no longer helps materially — the date-property
> store is identical in both modes and dwarfs the relationship-label store —
> so it lands at **~47 GiB**. Run the full specification only on a machine
> sized for it. On a laptop, scale down first:
>
> ```sh
> go run ./examples/26_social_scale_bench -users 20000 -articles 2000
> ```
>
> See [Memory profile and optimizations](#memory-profile-and-optimizations)
> for the per-edge breakdown and how these figures were measured.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-users` | `1000000` | number of `USER` nodes |
| `-articles` | `30000` | number of `ARTICLE` nodes |
| `-friends-min` | `150` | minimum `FRIEND` out-degree per user |
| `-friends-max` | `200` | maximum `FRIEND` out-degree per user |
| `-likes-max` | `300` | maximum `LIKE` out-degree per user |
| `-seed` | `1` | RNG seed (fixes the deterministic data shape) |
| `-rel-types` | `true` | store explicit `FRIEND`/`LIKE` types. `false`: infer the type from endpoint labels and store no per-edge label (much less memory; see below) |

## Expected output

Bare `key=value` lines carry **deterministic** facts (reproducible for a
fixed `-seed`); lines prefixed with `# ` carry **volatile telemetry**
(durations and heap figures that vary per run and per machine). A
representative scaled-down run
(`-users 20000 -articles 2000 -friends-min 150 -friends-max 200 -likes-max 300 -seed 1`):

```
config.users=20000
config.articles=2000
config.friends=[150,200]
config.likes=[0,300]
config.seed=1
config.rel_types=true
nodes.users=20000
nodes.articles=2000
edges.friend=3498025
edges.like=3006369
# build.elapsed=4.915s
# build.node_rate=4476 nodes/s
# build.edge_rate=1323475 edges/s
# mem.heap_alloc=988.60 MiB
# mem.heap_growth=988.25 MiB
# mem.total_alloc=2.32 GiB
# mem.sys=1.56 GiB
# mem.num_gc=18
# bytes_per_edge=159.3
q.count_users=20000
# q.count_users.latency=11.437ms
q.count_articles=2000
# q.count_articles.latency=1.088ms
q.count_friend=3498025
# q.count_friend.latency=4.466954s
q.count_like=3006369
# q.count_like.latency=4.101814s
q.friend_since_filled=3498025
# q.friend_since_filled.latency=9.20518s
q.like_when_filled=3006369
# q.like_when_filled.latency=8.380939s
q.fof_reach=15246
# q.fof_reach.latency=5.560177s
q.top_articles.rows=10
# q.top_articles.latency=11.000578s
```

The `edges.*` totals depend on the seed; `q.count_friend` and `q.count_like`
always equal `edges.friend` and `edges.like`, and the date-coverage counts
`q.friend_since_filled` / `q.like_when_filled` equal them in turn (every
relationship's date is filled) — the core consistency and always-filled
invariants the regression test asserts. The `# `-prefixed figures
(including all latencies) are environment-dependent and are **not** pinned
by the test.

## Memory profile and optimizations

The resident memory of this workload is dominated by the edges. A heap
profile of the build (captured with `go test -bench=BenchmarkBuild
-benchtime=1x -memprofile=mem.out` and read with `go tool pprof
-inuse_space`) originally showed that **~87 % of live heap was the
per-edge relationship-type storage** in `graph/lpg`: every labelled edge
allocated a whole `map[LabelID]struct{}` to hold, almost always, a single
label — even though this model has only two relationship types.

Three optimizations were applied (all verified against the full openCypher
TCK, the ACID battery, and `go test -race ./...` — no functional change):

1. **Compact single-label storage in `graph/lpg`.** The per-pair edge-label
   shard now stores the common single-label case as a bare `LabelID` inline
   in a `map[edgeKey]LabelID`, with a lazily-allocated spill map only for the
   rare multi-label edge. This removes the per-edge map allocation entirely
   while preserving multi-label semantics, the persistence format, and every
   public method's behaviour.
2. (Same structure as 1 — the inline `LabelID` is exactly the "no per-edge
   container" win, realised at the LPG layer so the durable CSR/snapshot
   format is untouched and durability/TCK invariants are preserved.)
3. **Implicit relationship types (`-rel-types=false`).** In this model the
   two edge kinds are already disambiguated by their endpoints — `FRIEND` is
   the only `USER→USER` edge and `LIKE` the only `USER→ARTICLE` edge — so the
   type can be inferred from endpoint labels and no per-edge label stored at
   all. The queries switch from `[:FRIEND]`/`[:LIKE]` to untyped `-->`.

### Measured effect (40k users / 4k articles ≈ 13 M edges, `inuse_space`)

| Version | Live heap | B/edge | vs. original |
|---|---:|---:|---:|
| Original (per-edge label map) | ~2.28 GiB | ~175 | — |
| Optimized, explicit types (1+2) | ~0.76 GiB | ~70 | **−66 %** |
| Optimized, implicit types (3) | ~0.31 GiB | ~24 | **−87 %** |

Confirmed with the full `runtime.ReadMemStats` heap at 60k/6k (≈19.5 M
edges): **3.60 GiB → 1.28 GiB** (explicit) and **→ 0.44 GiB** (implicit),
with identical query results in every mode. Extrapolated to the full
specification (~3.25 × 10⁸ edges): **~60 GiB → ~21 GiB** (explicit) or
**~7.4 GiB** (implicit) of live heap.

> These are *label-store* figures for a graph with **no** edge property:
> they predate both the per-node label-bag optimization and the mandatory
> `since`/`when` date property. With the date property every edge now carries,
> the current full-scale ceiling is **~48 GiB** — see
> [The mandatory date property now dominates resident heap](#the-mandatory-date-property-now-dominates-resident-heap)
> below.

### Update — per-edge labels moved into the adjacency column

A later optimization removed the per-pair edge-label map entirely. A heap
profile (`inuse_space`) of the explicit-type build at 40k/4k (≈13 M edges)
showed the `map[edgeKey]LabelID` store at **418.7 MiB = 56.7 % of live
heap (~32 B/edge)** — the single largest resident consumer, and one that
redundantly re-stored the `(src, dst)` pair the adjacency list already
holds. The relationship type is now stored inline as a compact per-slot
column in the adjacency entry (the same mechanism as the stable-handle
column), with a small spill structure for the rare multi-label edge and
for a label left on an already-removed edge. `AddEdgeLabeled` writes the
edge and its type together so the bulk build stays O(degree) amortised.

Measured at 40k/4k (explicit types), verified against the full openCypher
TCK (3897/3897), the ACID battery, and `go test -race ./...` — no
functional change and the on-disk snapshot format is unchanged:

| | Live heap | B/edge | Build |
|---|---:|---:|---:|
| Before (per-pair label map) | ~738 MiB | ~57 | ~7.5 s |
| After (inline label column) | **~375 MiB** | **~29** | **~3.3 s** |

The edge-label store dropped **~85 %** (418.7 → ~60 MiB) and total live
heap **~49 %**; build time also improved because the fused
`AddEdgeLabeled` path drops the per-edge existence check the old
`AddEdge` + `SetEdgeLabel` pair paid.

### The mandatory date property now dominates resident heap

The optimizations above shrank the relationship-**label** store to a few
bytes per edge. Adding the mandatory `since`/`when` **date property** to
every edge reverses that win in absolute terms: a pair-level edge property
is held in a per-edge keyed map (`map[edgeKey]propBag`), and even the
compact `propBag` — a small inline slice for the common 1-2-property edge,
not a nested map — still costs far more per edge than the date string it
holds, because each edge pays a map slot keyed by `(src, dst)` plus a
one-element slice plus an interface box around the value.

Measured at the 20k/2k scale of the *Expected output* block above (≈6.5 M
edges), a heap profile (`inuse_space`) of the build attributes the live
heap between the date-property store and everything else, with explicit
relationship types:

| | Live heap | B/edge |
|---|---:|---:|
| Everything except the date property (adjacency + labels + nodes) | ~187 MiB | ~29 |
| The `since`/`when` date-property store | **~833 MiB** | **~128** |
| **Total** | **~1020 MiB** | **~157** |

The date-property store is **~128 B/edge**, of which only **~16 B** is the
ISO date string itself; the other **~112 B** is structural overhead — the
`map[edgeKey]propBag` slot (~65 B), the one-element slice backing (~31 B),
and the `any`-interface box around the value (~17 B). Two consequences
follow:

1. **Edge properties, not labels, now set the memory ceiling.** Extrapolated
   to the full specification (~3.25 × 10⁸ edges) the live heap is **~48 GiB**,
   of which the edge-property store alone is **~39 GiB**.
2. **The implicit-type mode (`-rel-types=false`) no longer saves meaningfully.**
   The date-property store is identical in both modes and dwarfs the
   label store, so implicit types measure **~156 B/edge (~1.01 GiB)** at
   20k/2k — within ~2 % of explicit. The flag still changes how the
   relationship *kind* is encoded and queried, but it is no longer a memory
   lever once every edge carries a property.

This is intrinsic to storing a queryable edge property with the current
engine: there is no inline/columnar edge-property tier analogous to the
relationship-label column, so the per-edge map slot, slice, and interface
box dominate — not the value (a lighter value encoding alone does not help).
A position-aligned columnar edge-property tier — the same mechanism the
relationship-label column already uses — would remove that overhead and is
tracked in the project backlog. `lpg.TimeValue` was rejected for a
different reason — the Cypher reader maps it to null, so it would not be
queryable as `r.since` / `r.when` at all.

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `Graph.SetNodeLabel` / `Graph.SetNodeProperty` — build the labelled property graph in memory.
- `graph/lpg.Graph.AddEdgeLabeledWithProperty` — add a typed `FRIEND` / `LIKE` relationship **and** its mandatory `since` / `when` date in one call: the edge, its relationship type, and the date property are all written into the new adjacency slot at insertion time, so the bulk build stays O(degree) amortised per source. This fuses the relationship-type inline label column and the columnar edge-property tier in a single append, avoiding the per-edge column copy a separate `SetEdgeProperty` would pay (which made a bulk property-carrying build O(degree²) per source). `Graph.AddEdgeLabeled` / `Graph.AddEdge` / `Graph.SetEdgeLabel` remain for the untyped and re-labelling cases.
- `graph/lpg.Graph.SetEdgeProperty` — set or mutate a relationship property on a pair after the edge exists (a pair-level property, which is unambiguous here because every endpoint pair carries one edge; it is the tier the Cypher engine reads as `r.since` / `r.when`). The bulk build uses the fused `AddEdgeLabeledWithProperty` instead; `SetEdgeProperty` is the general single-property path used by the untyped branch.
- `graph/lpg.StringValue` — wrap string property values, including the ISO-8601 date strings stored on relationships.
- `cypher.NewEngine` / `Engine.Run` — query the in-memory graph.
- `cypher.Result.Next` / `Result.Record` / `Result.Err` / `Result.Close` — iterate result rows and read columns.
- `cypher/expr.StringValue` / `expr.IntegerValue` — typed query parameters and result cells.
- `runtime.ReadMemStats` — capture the Go heap footprint of the build.

## Further reading

- [`graph/lpg`](../../graph/lpg) — labelled property graph package
- [`cypher`](../../cypher) — Cypher engine package
- [Example 22 — Cypher](../22_cypher) — the Cypher engine over a small graph
- [Example 11 — Social network](../11_social_network) — analytics over a small social LPG
- [Example 24 — Social-network CLI](../24_social_network_cli) — a persistent social-network store
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
