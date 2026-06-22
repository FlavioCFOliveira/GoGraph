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
records when the like happened, both always present. They are written with
`lpg.DateValue`, which stores a **native Cypher `Date`**: the storage tier
folds it into a compact **int32 epoch-day column** (~4 bytes/value) and the
engine reads it back as a `Date`, so range and `ORDER BY` predicates over
`since`/`when` behave as dates natively. (`lpg.TimeValue` is deliberately
**not** used here: the Cypher reader maps `PropTime` to null; and a plain
ISO-8601 string would read back as a `String` and cost a ~16-byte header
plus its backing text — that per-edge string cost is what `#1649` removed
by switching to `DateValue`.) The dates are drawn from the seeded RNG
anchored to a fixed reference date — never the wall clock — so they are
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

> **Resource warning.** Each edge carries a mandatory date property. At this
> model's degrees the graph needs on the order of **~62 bytes of live heap per
> edge** (measured at 20k/2k, explicit types — see below), so the full run needs
> roughly **~20 GiB of live heap** and a few minutes to build. The implicit-type
> mode (`-rel-types=false`) does not change this materially — the date-property
> columns are identical in both modes and the relationship-label column is already
> negligible. Run the full specification only on a machine sized for it. On a
> laptop, scale down first:
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
# build.elapsed=3.013s
# build.node_rate=7302 nodes/s
# build.edge_rate=2158801 edges/s
# mem.heap_alloc=151.57 MiB
# mem.heap_growth=151.22 MiB
# mem.total_alloc=3.93 GiB
# mem.sys=474.36 MiB
# mem.num_gc=68
# bytes_per_edge=24.4
q.count_users=20000
# q.count_users.latency=11.466ms
q.count_articles=2000
# q.count_articles.latency=999µs
q.count_friend=3498025
# q.count_friend.latency=4.236896s
q.count_like=3006369
# q.count_like.latency=3.859588s
q.friend_since_filled=3498025
# q.friend_since_filled.latency=8.453554s
q.like_when_filled=3006369
# q.like_when_filled.latency=7.878798s
q.fof_reach=15246
# q.fof_reach.latency=5.21854s
q.top_articles.rows=10
# q.top_articles.latency=9.813192s
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

### Baseline: the date property in the map-backed store

The optimizations above shrank the relationship-**label** store to a few
bytes per edge. Adding the mandatory `since`/`when` **date property** to
every edge originally reversed that win in absolute terms. In the first
implementation a pair-level edge property was held in a per-edge keyed map
(`map[edgeKey]propBag`), and even the compact `propBag` — a small inline
slice for the common 1-2-property edge, not a nested map — cost far more per
edge than the date string it holds, because each edge paid a map slot keyed
by `(src, dst)` plus a one-element slice plus an interface box around the
value. The figures below are that **pre-columnar baseline**; the columnar
tier that replaced it is documented immediately after.

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

`lpg.TimeValue` was rejected for a different reason — the Cypher reader maps
it to null, so it would not be queryable as `r.since` / `r.when` at all. The
map-backed baseline stored the date as an ISO-8601 string; it is now written
with `lpg.DateValue` and folded into the `int32` epoch-day column (`#1649`,
see below).

### The columnar edge-property tier

The map-backed store above was replaced by a **position-aligned columnar
edge-property tier** (design:
[`docs/columnar-edge-properties-design.md`](../../docs/columnar-edge-properties-design.md)).
Edge property values now live in per-`(propertyKeyID, kind)` de-boxed typed
columns co-located with the adjacency `neighbours` array — the same mechanism
the relationship-label column uses — so the redundant `(src, dst)` map key,
the per-edge slice, and the interface box are all gone. A column carries an
Arrow-style validity bitmap only where some slots are absent (a fully dense
column pays none), and a column that is sparse within a high-degree node's
neighbour list (here `since` is set only on `FRIEND` slots and `when` only on
`LIKE` slots) switches to a compact COO representation. The date round-trips
through Cypher as a non-null value (never `lpg.PropTime`, which the reader
maps to null).

Measured at 20k/2k (≈6.5 M edges), explicit types, full `runtime.ReadMemStats`,
verified against the full openCypher TCK (3897/3897), the ACID battery, and
`go test -race ./...` — identical query results:

| | Live heap | B/edge | vs. baseline |
|---|---:|---:|---:|
| Map-backed baseline | ~1020 MiB | ~157 | — |
| Columnar tier (dense columns) | ~465 MiB | ~74.8 | **−53 %** |
| Columnar tier + sparse COO | **~383 MiB** | **~61.8** | **−61 %** |

Extrapolated to the full specification (~3.25 × 10⁸ edges) the live-heap
ceiling drops from the baseline **~48 GiB to ~20 GiB**. Build throughput is
preserved: each edge's date is written into the column at insertion time via
`AddEdgeLabeledWithProperty` (a fused append), so a bulk property-carrying
build stays O(degree) amortised per source rather than the O(degree²) a
separate per-edge `SetEdgeProperty` would pay — build allocation stays within
~1.8× of the label-only baseline (~4.2 GiB total alloc at 20k/2k, vs ~2.3 GiB)
rather than the ~54 GiB an un-fused build incurs.

### The int32 epoch-day date column (`#1649`)

The columnar tier already implemented a typed `int32` epoch-day date column;
`#1649` activated it for the Go-API build path. Dates are now written with
`lpg.DateValue` (a Cypher-visible `Date`) instead of a plain ISO-8601 string,
so the value folds into the `int32` column (~4 bytes/value) rather than the
string column (a ~16-byte header plus its backing text). Measured at 20k/2k
(≈6.5 M edges), explicit types, full `runtime.ReadMemStats`, verified against
the full openCypher TCK (3897/3897) and `go test -race` — identical query
results (the `since`/`when` coverage counts are unchanged):

| | Live heap | B/edge | vs. ISO-string column |
|---|---:|---:|---:|
| ISO-8601 string date column | ~383 MiB | ~61.8 | — |
| `int32` epoch-day column (`DateValue`) | **~204 MiB** | **~32.9** | **−47 %** |

Extrapolated to the full specification (~3.25 × 10⁸ edges) the live-heap
ceiling drops from **~20 GiB to ~10 GiB**.

### The weightless adjacency mode (`#1650`)

This social graph is queried only by relationship type and property — never by
edge weight — yet the Cypher engine forces `W = float64`, so the adjacency's
per-edge weight column (8 bytes/edge) is dead. Building the graph with
`adjlist.Config{Weightless: true}` drops that column entirely: `AddEdge`'s weight
argument is accepted but ignored, reads return the zero weight, and the
CSR/snapshot persist no weights (the manifest records `weightless`, so a
recovered graph stays weightless rather than re-allocating a zero column).
Measured at 20k/2k, verified against the full openCypher TCK (3897/3897), `go
test -race`, and a snapshot+recovery round-trip:

| | Live heap | B/edge | vs. weighted |
|---|---:|---:|---:|
| int32 date column, weighted | ~204 MiB | ~32.9 | — |
| int32 date column, weightless (`#1650`) | **~151 MiB** | **~24.4** | **−26 %** |

The two edge-store optimizations together take this example from the ISO-string
weighted baseline of ~61.8 B/edge to **~24.4 B/edge (−60 %)**; the full-scale
(~3.25 × 10⁸ edges) live-heap ceiling drops from ~20 GiB to **~8 GiB**.
Remaining headroom: a global dense edge-record (gated on a universal edge id)
would suit edge-centric workloads — tracked in the backlog.

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `Graph.SetNodeLabel` / `Graph.SetNodeProperty` — build the labelled property graph in memory.
- `graph/lpg.Graph.AddEdgeLabeledWithProperty` — add a typed `FRIEND` / `LIKE` relationship **and** its mandatory `since` / `when` date in one call: the edge, its relationship type, and the date property are all written into the new adjacency slot at insertion time, so the bulk build stays O(degree) amortised per source. This fuses the relationship-type inline label column and the columnar edge-property tier in a single append, avoiding the per-edge column copy a separate `SetEdgeProperty` would pay (which made a bulk property-carrying build O(degree²) per source). `Graph.AddEdgeLabeled` / `Graph.AddEdge` / `Graph.SetEdgeLabel` remain for the untyped and re-labelling cases.
- `graph/lpg.Graph.SetEdgeProperty` — set or mutate a relationship property on a pair after the edge exists (a pair-level property, which is unambiguous here because every endpoint pair carries one edge; it is the tier the Cypher engine reads as `r.since` / `r.when`). The bulk build uses the fused `AddEdgeLabeledWithProperty` instead; `SetEdgeProperty` is the general single-property path used by the untyped branch.
- `graph/lpg.DateValue` — wrap the mandatory `since` / `when` relationship dates as native Cypher `Date` values; folds into the compact `int32` epoch-day column (~4 bytes/value) and reads back as a `Date` (`#1649`).
- `graph/lpg.StringValue` — wrap string property values (node `id` / `name`, article `title`).
- `graph/adjlist.Config{Weightless: true}` (passed through `lpg.New`) — build a graph with no per-edge weight column, for a workload queried only by relationship/property; `AddEdge`'s weight argument is ignored and reads return the zero weight. Persisted in the snapshot manifest so a recovered graph stays weightless (`#1650`).
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
