# Example 26 — Social-network scale benchmark

## What it demonstrates

Building a large labelled property graph that models a social network and
measuring **query performance** and **resource consumption** over it: the
example reports build throughput, Go heap footprint, and the latency of a
broad battery of representative Cypher queries. The battery spans three
groups:

- **Counts and traversal** — label-scan counts, relationship counts, the
  always-filled date-coverage counts, a friend-of-friend traversal, and a
  trending-articles grouped aggregation.
- **Analytical aggregation and subqueries** — the friend out-degree
  distribution via `min` / `max` / `avg` and `percentileCont` (median); an
  `EXISTS { }` / `NOT EXISTS { }` subquery split; a `CASE` bucketing
  projection; a `UNION ALL` of two label-count streams; an `UNWIND $ids`
  batch point-read; and `id()` / `elementId()` on a matched node.
- **Temporal functions** — `date()` / `datetime()` constructors, the
  `duration.between` family (`duration.inDays` / `inSeconds`) with duration
  component access, `Date − Duration` arithmetic, and `date.truncate` — all
  anchored to a **fixed reference date** so the results stay deterministic.
- **Planner statistics and cardinality estimates** — after building the graph
  it calls `Engine.RefreshStatistics` (the single, caller-driven statistics
  rebuild) and then prints the `EXPLAIN` plan of three representative queries,
  each operator annotated with its estimated row count and **provenance** —
  `exact` (a maintained count), `heuristic` (the `1/NDV` distribution average),
  or `stats` (an equi-depth histogram estimate with its certified error). It
  closes with the statistics-health telemetry: the tracked-`(label, property)`-
  pair count, the refresh latency, and the **estimate-vs-actual** accuracy of
  both an exact label scan and the approximate range.
- **Columnar execution and its allocation win** — three analytic queries that
  each engage one of the engine's columnar (chunk-at-a-time) physical operators
  — a `GROUP BY` aggregation, a filter-over-traversal, and a disconnected
  equi-join — measured against a result-identical row-at-a-time baseline so the
  **de-boxing allocation win** each delivers is observable. See
  [Columnar execution exercise](#columnar-execution-exercise-2121).
- **Automatic intra-query parallelism** — over a user population large enough to
  cross the parallel-scan threshold, a whole-graph `min` / `max` aggregate is run
  on a parallel-configured engine and on a serial one, reporting the **speedup**
  after verifying the two results are bit-identical; and a `count(*)` over every
  node is served by the **`O(1)` count pushdown**, contrasted against the `O(N)`
  scan it replaces. The parallel win is idle-core-bound and reported as
  single-tenant telemetry. See
  [Intra-query parallelism exercise](#intra-query-parallelism-exercise-2122).

## Domain / scenario

A social network of `USER` and `ARTICLE` nodes:

```
(:USER    {id, name, country})         // id is a 24-char hex string, name is realistic
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

Each user also carries a low-cardinality categorical `country` — the group-by
key of the [columnar execution exercise](#columnar-execution-exercise-2121). It
is derived **deterministically from the user index**, never from the RNG, so it
adds a realistic analytics dimension without perturbing the seeded stream: every
degree, like, and date fact is byte-for-byte what it was before the dimension
existed.

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
| `-profile-dir` | `""` | if set, write `cpu.pprof` and `heap.pprof` here. Inspect with `go tool pprof -http=:0 <file>` |

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
# build.elapsed=3.847s
# build.node_rate=5719 nodes/s
# build.edge_rate=1690865 edges/s
# mem.heap_alloc=152.55 MiB
# mem.heap_growth=152.20 MiB
# mem.total_alloc=9.12 GiB
# mem.sys=417.48 MiB
# mem.num_gc=125
# bytes_per_edge=24.5
q.count_users=20000
# q.count_users.latency=2.494ms
q.count_articles=2000
# q.count_articles.latency=119µs
q.count_friend=3498025
# q.count_friend.latency=4.212704s
q.count_like=3006369
# q.count_like.latency=3.848539s
q.friend_since_filled=3498025
# q.friend_since_filled.latency=5.476491s
q.like_when_filled=3006369
# q.like_when_filled.latency=4.957594s
q.fof_reach=15246
# q.fof_reach.latency=175.579ms
q.top_articles.rows=10
# q.top_articles.latency=7.150727s
q.friend_degree.min=150
q.friend_degree.max=200
q.friend_degree.avg=174.9013
q.friend_degree.median=175.0000
# q.friend_degree.latency=8.303789s
q.users_with_like=19927
# q.users_with_like.latency=172.137ms
q.users_without_like=73
# q.users_without_like.latency=167.935ms
q.degree_band.high=6577
q.degree_band.low=6745
q.degree_band.mid=6678
# q.degree_band.latency=8.472216s
q.union.rows=2
q.union.users=20000
q.union.articles=2000
# q.union.latency=11.326ms
q.unwind_requested=8
q.unwind_matched=8
# q.unwind_batch.latency=11.064ms
q.sample_node_id=122
q.sample_element_id=122
# q.id_pair.latency=4.63ms
q.temporal.window_days=2192
# q.temporal.window_days.latency=121µs
q.temporal.dt_span_seconds=5400
# q.temporal.dt_span_seconds.latency=53µs
q.friend_age_days.min=0
q.friend_age_days.max=2192
# q.friend_age_days.latency=9.842523s
q.friend_recent_30d=49502
# q.friend_recent_30d.latency=6.723331s
q.friend_by_year.2019=582806
q.friend_by_year.2020=583777
q.friend_by_year.2021=582547
q.friend_by_year.2022=580427
q.friend_by_year.2023=583308
q.friend_by_year.2024=583561
q.friend_by_year.2025=1599
# q.friend_by_year.latency=7.309185s
# --- planner statistics & cardinality estimates (#2120) ---
# stats.tracked_pairs=5
# stats.refresh.latency=41.2ms
# stats.explain.label_scan:
#   ProduceResults
#   └─ Projection
#      └─ NodeByLabelScan [u:USER] (est. rows=20000, exact)
# stats.explain.equality_1_over_ndv:
#   ProduceResults
#   └─ Projection
#      └─ Selection (est. rows~1, heuristic)
#         └─ NodeByLabelScan [u:USER] (est. rows=20000, exact)
# stats.explain.range_histogram:
#   ProduceResults
#   └─ Projection
#      └─ Selection (est. rows~13648, stats, err=0.0039)
#         └─ NodeByLabelScan [u:USER] (est. rows=20000, exact)
# stats.label.est_rows=20000
# stats.label.actual_rows=20000
# stats.range.est_rows=13648
# stats.range.actual_rows=13672
# stats.range.abs_row_error=24
# --- columnar execution exercise (#2121) ---
# columnar.scale.users=1500
# columnar.scale.articles=200
# columnar.agg.groups=12
# columnar.agg.members_total=1500
# columnar.agg.col_mallocs=4406
# columnar.agg.row_mallocs=17899
# columnar.agg.col_bytes=149104
# columnar.agg.row_bytes=1246480
# columnar.agg.malloc_ratio=4.06
# columnar.filter.rows=9010
# columnar.filter.col_batches=3
# columnar.filter.row_batches=0
# columnar.filter.col_mallocs=123
# columnar.filter.row_mallocs=120882
# columnar.filter.col_bytes=1516592
# columnar.filter.row_bytes=2709008
# columnar.filter.malloc_ratio=982.78
# columnar.hashjoin.pairs=457
# columnar.hashjoin.col_mallocs=73448
# columnar.hashjoin.nested_mallocs=8646912
# columnar.hashjoin.col_bytes=5631192
# columnar.hashjoin.nested_bytes=146680528
# columnar.hashjoin.malloc_ratio=117.73
# --- intra-query parallelism exercise (#2122) ---
# parallel.scale.nodes=60000
# parallel.scale.threshold=1024
# parallel.gomaxprocs=10
# parallel.min.value=0
# parallel.min.parallel_elapsed=8.383ms
# parallel.min.serial_elapsed=28.767ms
# parallel.min.speedup=3.43
# parallel.max.value=99997
# parallel.max.parallel_elapsed=9.385ms
# parallel.max.serial_elapsed=29.236ms
# parallel.max.speedup=3.12
# parallel.count.nodes=60000
# parallel.count.value=60000
# parallel.count.o1_elapsed=6.458µs
# parallel.count.scan_elapsed=12.4ms
# parallel.count.speedup=1920.1
```

The `edges.*` totals depend on the seed; `q.count_friend` and `q.count_like`
always equal `edges.friend` and `edges.like`, and the date-coverage counts
`q.friend_since_filled` / `q.like_when_filled` equal them in turn (every
relationship's date is filled) — the core consistency and always-filled
invariants the regression test asserts. The added battery pins further
deterministic invariants:

- **Analytical (`#1971`).** `q.friend_degree.{min,max}` bracket the configured
  degree range and `q.friend_degree.{avg,median}` summarise the distribution;
  `q.users_with_like` + `q.users_without_like` = `nodes.users` (the
  `EXISTS { }` / `NOT EXISTS { }` split); the `q.degree_band.*` counts (bands
  are the equal-width tertiles of the degree range) sum to `nodes.users`;
  `q.union.{users,articles}` restate the label totals over two `UNION ALL`
  streams; `q.unwind_matched` = `q.unwind_requested` (every batched id is
  real); and `q.sample_element_id` is the decimal string form of the integer
  `q.sample_node_id` (`id()` is an Integer, `elementId()` a String — the id
  value itself is deterministic for the seed but implementation-defined).
- **Temporal (`#1972`).** `q.temporal.window_days=2192` and
  `q.temporal.dt_span_seconds=5400` are constructor sanity checks with known
  answers; `q.friend_age_days.{min,max}` span the whole edge-date window
  `[0, 2192]`; `q.friend_recent_30d` is the friendships dated within 30 days
  of the fixed reference date (`Date − Duration` arithmetic); and the
  `q.friend_by_year.*` buckets (`date.truncate('year', …)`) sum to
  `edges.friend`.
- **Planner statistics (`#2120`).** The `stats.*` block is telemetry (every
  line is `# `-prefixed), because it renders the planner's internal estimate
  model rather than a data-shape fact. `stats.tracked_pairs=5` is the schema's
  five `(label, property)` pairs — `(USER,id)`, `(USER,name)`, `(USER,country)`,
  `(ARTICLE,id)`, `(ARTICLE,title)`. The three `stats.explain.*` blocks show one estimate per
  provenance class: the label scan is `exact` (from the label index); the
  equality on the high-cardinality `name`, against a value the generator never
  produces, is the `1/NDV` `heuristic`; and the `name < 'M'` range is the
  equi-depth histogram estimate tagged `stats` with the certified absolute
  selectivity error `err=1/B ≈ 0.0039` (`B = 256` buckets, and Δ = 0 right after
  a rebuild). The accuracy pair confirms the tags: the label scan's estimate
  equals the real count exactly (`stats.label.est_rows` = `stats.label.actual_rows`),
  while the range estimate lands within the histogram's guarantee of the true
  count (`stats.range.abs_row_error` well under `users/B`). The estimates are
  **display-only** — they annotate `EXPLAIN` but never change which plan runs.
- **Columnar execution (`#2121`).** The `columnar.*` block reports, for each of
  the three columnar operators, the allocation profile of the columnar execution
  and of a result-identical row-mode baseline; `*.malloc_ratio` is
  `baseline / columnar`. `columnar.scale.*` is the bounded working set the
  exercise builds (never the full `-users` scale — see
  [Columnar execution exercise](#columnar-execution-exercise-2121)), so these
  figures are the same at any `-users`. `columnar.filter.col_batches > 0` with
  `columnar.filter.row_batches == 0` is the direct engagement proof: the engine
  drove its columnar-filter path for the columnar query and not for the
  `coalesce()` baseline.
- **Intra-query parallelism (`#2122`).** The `parallel.*` block is telemetry (every
  line is `# `-prefixed). `parallel.scale.nodes` is the fixed working set the
  exercise builds and `parallel.scale.threshold` the lowered
  `ParallelScanThreshold` above which the parallel path engages. `parallel.min` /
  `parallel.max` report the aggregate `value` — identical between the parallel and
  serial engine, or the exercise fails the run — with the parallel and serial
  wall-clocks and their `speedup`. `parallel.count.value` is the `O(1)` count
  pushdown result, equal to the population and to the `O(N)` scan it is contrasted
  with; `o1_elapsed` versus `scan_elapsed` makes the `O(1)` nature visible.

The `# `-prefixed figures (including all latencies and the whole `stats.*`,
`columnar.*`, and `parallel.*` blocks) are environment-dependent and are **not**
pinned by the test — except the statistics, columnar, and parallelism tests read the
deterministic values by name to assert the provenance tags, the tracked-pair count,
the estimate-vs-actual accuracy, the columnar engagement, the **direction** of each
allocation win, and — for the parallelism exercise — the result identity and the exact
population count (never a timing or the speedup, which are idle-core-bound).

## Columnar execution exercise (#2121)

The engine executes qualifying query shapes with **columnar (chunk-at-a-time)**
physical operators that carry values in typed, column-major chunks and evaluate
over them **unboxed**, instead of boxing every value into an `interface`-typed
row cell. This exercise drives three analytic queries that each engage one such
operator and measures the **allocation win** the de-boxing delivers, so the
example does not merely *use* the columnar path but produces evidence that it
engaged and that it allocates less.

**Scenario.** Realistic social-graph analytics: *how many members per country*,
*which relationships point at a destination in a given id range*, and *which
users share a display name* (a duplicate-account / homonym check).

**Objective.** Show, empirically, that each columnar operator returns a result
**identical** to the row-at-a-time path while allocating materially less — and
prove the columnar path actually engaged, rather than assuming it.

**Purpose.** Give the columnar operators a measurable, reproducible harness so a
regression that silently drops back to the boxed row path (or erodes the win)
is caught.

### The three queries

| Query | Cypher | Columnar operator engaged |
|---|---|---|
| Aggregation | `MATCH (u:USER) RETURN u.country, count(*) …` | columnar aggregation — hashes the grouping key **unboxed**, boxing it only once per distinct group (`#2049`/`#2104`) |
| Filter-over-traversal | `MATCH (u)-[r]->(p) WHERE p.id >= '8' RETURN p.id` | columnar `Expand` + `ColumnarFilter` — reads the far node's id and evaluates the predicate over the traversal output **unboxed** (`#2106`) |
| Disconnected equi-join | `MATCH (a:USER),(b:USER) WHERE a.name = b.name AND a.id < b.id RETURN count(*)` | columnar hash join — retains build-side rows in a **column-major** buffer instead of a per-row snapshot (`#2105`) |

The filter query's pattern is deliberately **bare** (no labels or relationship
types): a label or type on either endpoint inserts a `Selection` that breaks the
`scan → Expand → ColumnarFilter` chain, so the columnar path would not engage. A
label on the far node would still return the right answer via the boxed path —
which is exactly why the exercise verifies engagement rather than trusting it.

### How the win is measured

Each columnar query is paired with a **result-identical row-mode baseline** and
both allocation profiles are reported:

- the aggregation and filter baselines wrap the property in `coalesce(…)`, which
  is value-equivalent (the property is always present) but disqualifies the
  columnar pre-projection / predicate, forcing the boxed row path;
- the join baseline runs the identical query on an engine constructed with
  `EngineOptions{DisableHashJoin: true}`, i.e. the `O(V²)` nested-loop plan.

Each pair is asserted to return the **identical** result before its allocation
delta is reported, so a divergence fails loudly rather than flattering the win.
Allocations are read from monotonic, GC-independent `runtime.MemStats` counters
(`Mallocs`, `TotalAlloc`) around a **warmed** execution (the one-off plan
compilation is excluded), and the query is drained through `Result.Next` alone —
never `Record` — so the figure reflects the **operator's** execution allocations
(where the de-box lives), not the per-row map a caller would add identically to
both paths.

The columnar `Expand` + `ColumnarFilter` engagement is confirmed directly: the
exec package increments a counter once per columnar-filter batch, and the
exercise reads it back (`columnar.filter.col_batches`) to prove the columnar
query drove that path and the `coalesce()` baseline did not.

### Bounded working set

The row-mode baselines allocate in proportion to the rows they touch, and the
nested-loop join is `O(V²)`, so running them at the full `-users` scale would be
ruinous. The exercise therefore builds its **own bounded sub-graph** — capped at
`colScaleUsers` users / `colScaleArticles` articles — rather than reusing the
main dataset. The columnar operators it drives are the same ones the main battery
uses at full scale; only their allocation behaviour is measured here, where a
controlled row-mode baseline is affordable. The bounded scale makes the
`columnar.*` figures identical at any `-users`, and keeps the whole exercise (and
its race-detector run) fast.

### Observed win (bounded working set, `colScaleUsers = 1500`)

| Operator | Row-mode allocs | Columnar allocs | Ratio |
|---|---:|---:|---:|
| Aggregation (`count(*)` per country) | ~17,900 | ~4,400 | **~4×** |
| Filter-over-traversal (~9,000 rows) | ~120,900 | ~120 | **~980×** |
| Disconnected equi-join (nested-loop baseline) | ~8,650,000 | ~73,400 | **~118×** |

The filter is the most striking: the columnar `Expand` + filter path is
effectively **zero-allocation per row** (a fixed ~120 allocations regardless of
the ~9,000 rows it streams), whereas the boxed baseline allocates roughly one
boxed value per surviving row. Exact figures are environment-dependent; the
regression test asserts only the **direction** of each win (and the filter
engagement counter), never a pinned value.

### Exercised GoGraph APIs

`cypher.NewEngine` / `cypher.NewEngineWithOptions` (with
`EngineOptions{DisableHashJoin: true}` for the baseline), `Engine.Run`,
`Result.Next` / `Result.Record` / `Result.Err` / `Result.Close`, and the
`internal/metrics` backend hook (`metrics.SetBackend`) to read the columnar-filter
batch counter. The exercise adds **no** module code: the columnar operators engage
automatically on the qualifying query shapes.

## Intra-query parallelism exercise (#2122)

The engine parallelises some whole-graph aggregates **automatically**, and answers
others in constant time, once the graph is large enough to make it worthwhile. This
exercise drives both paths and produces evidence they engaged and are correct:

- the morsel-parallel **`min` / `max` aggregate** (`#2111`) — a group-by-less
  `min` / `max` over a bare full-node scan whose per-node property read and `Compare`
  are split across up to `GOMAXPROCS` workers;
- the **`O(1)` `count(*)` pushdown** (`#2113`) — a group-by-less `count(*)` over a
  bare full-node scan that reads the maintained live-node counter directly, without
  walking a single node.

**Scenario.** Network-wide analytics over a large **user population**, each account
carrying a numeric `reputation` score: *what are the lowest and highest reputation in
the whole network*, and *how many accounts are there in total*. These are whole-graph
node aggregates — they never traverse an edge — so the exercise's working set is a
pure account population (edge traversal is exercised by the main battery and the
columnar exercise).

**Objective.** Show, empirically, that the parallel `min` / `max` returns a result
**bit-identical** to the serial path while completing faster on an idle box, and that
the `count(*)` pushdown returns the exact population in `O(1)` — a wall-clock that does
not scale with the node count.

**Purpose.** Give the parallel aggregate and the count pushdown a measurable,
reproducible harness so a regression that diverges the parallel result, or that turns
the `O(1)` count back into an `O(N)` scan, is caught.

### The two contrasts

| Query | Cypher | Engaged path | Contrasted against |
|---|---|---|---|
| Whole-graph `min` / `max` | `MATCH (n) RETURN min(n.reputation)` | morsel-parallel aggregate (`ParallelScanThreshold` lowered so it engages) | the **same query** on an `EngineOptions{DisableParallelScan: true}` serial engine |
| Whole-graph `count(*)` | `MATCH (n) RETURN count(*)` | `O(1)` `AllNodesCountScan` (serial engine) | `MATCH (n) WHERE n.reputation >= 0 RETURN count(*)`, whose trivially-true `WHERE` keeps the scan non-bare and forces the `O(N)` full-scan count |

The `min` / `max` pattern is deliberately **bare** (`MATCH (n)`, no label): a label
turns the scan into a `NodeByLabelScan`, a different operator. The count baseline's
`WHERE n.reputation >= 0` is always true (reputation is drawn from `[0, 100000)`), so
it counts the identical population while denying the pushdown its bare-scan shape.

### How the win is measured

The parallel and serial engines share the **same immutable population**, so the scan
order is identical and the parallel reduce — which carries each partial extremum with
the scan position that breaks a `Compare`-tie — is **bit-identical** to the serial
first-seen result, not merely value-equal. Each query is warmed (excluding one-off plan
compilation) and then timed as the best of a few repetitions, which damps scheduler
noise without pinning a machine-specific number. Every `Result` is fully drained and
closed, so the parallel worker pool joins before the exercise returns (the package's
`goleak` `TestMain` enforces it). The two aggregate values are asserted **identical**,
and the `O(1)` count, the `O(N)` scan, and the known population are asserted to agree,
**before** any timing is reported — a divergence fails the run loudly.

### Observed figures (idle box, `parScaleUsers = 60000`, `GOMAXPROCS = 10`)

| Contrast | Serial / `O(N)` | Parallel / `O(1)` | Speedup |
|---|---:|---:|---:|
| `min(n.reputation)` | ~28.8 ms | ~8.4 ms | **~3.4×** |
| `max(n.reputation)` | ~29.2 ms | ~9.4 ms | **~3.1×** |
| `count(*)` (`O(N)` scan → `O(1)` read) | ~12.4 ms | ~6.5 µs | **~1900×** |

**Honest telemetry — the win is idle-core-bound.** Intra-query parallelism is a
**latency** win that pays only by consuming otherwise-idle cores: speedup ≈
`min(workers, idle cores) × efficiency`. The figures above are the **single-tenant**
case (one query at a time on an idle box), which is exactly this example's run. Under
concurrent multi-client load the engine's shared worker governor correctly throttles
each query toward the serial path (a `budget == 1` short-circuit runs the reduce
inline), so the win narrows toward **parity** — no regression, but no speedup either.
This exercise does not model that regime and does not claim the speedup holds under
saturation. Accordingly the regression test asserts the deterministic invariants
(result identity, the exact count) but **never** that the parallel arm was faster.

### Bounded working set

The parallel path engages only above `ParallelScanThreshold` live nodes, so the
exercise builds a population large enough to cross a lowered threshold and to give the
reduce several morsels of work — a **fixed** `parScaleUsers`, not the `-users` scale,
because intra-query parallelism only matters at scale. The population is small enough
that the whole exercise (build plus timed queries) stays a few seconds under the race
detector in the short test layer, and its figures are identical at any `-users`.

### Exercised GoGraph APIs

`cypher.NewEngineWithOptions` with `EngineOptions{ParallelScanThreshold: …}` (parallel)
and `EngineOptions{DisableParallelScan: true}` (serial) — `ParallelScanThreshold` is an
engine **configuration** knob, not a module change; the shipped default is
`cypher.DefaultParallelScanThreshold` (50,000). Also `Engine.Run`, `Result.Next` /
`Result.Record` / `Result.Err` / `Result.Close`, and the `graph/lpg` builders
(`AddNode`, `SetNodeLabel`, `SetNodeProperty` with `lpg.Int64Value` / `lpg.StringValue`,
`AdjList().Compact`). The exercise adds **no** module code: the parallel aggregate and
the count pushdown engage automatically on the qualifying query shapes and scale.

## CSR neighbour-ordering exercise (#2147)

### Scenario

Sprint 313 made every CSR source's neighbour run ordered by the total key
`(destination, handle)`, so the executor's forward-position membership probes
binary-search instead of scanning. This section exercises that on a
realistically-sized social graph and reports what the example's own data shape
means for it.

### Objective

**To show the ordered path engaging and not regressing — and to be explicit that
this example cannot demonstrate a hub win.** That honesty is the point of the
section, not a caveat bolted onto it.

The example generates a **bounded, light-tailed** out-degree: each user draws a
`FRIEND` degree uniformly from `[friends-min, friends-max]` and a `LIKE` degree
uniformly from `[0, likes-max]`. Every user is therefore statistically the same
size and there is **no hub tail at all**. A real social graph is power-law, where
a few vertices carry a disproportionate share of traversal cost. Demonstrating
the hub win is [`bench/csrorder`](../../bench/csrorder)'s job, on a
Barabási–Albert fixture; this section's job is to state plainly what shape the
example actually has, so a uniform result is never mistaken for a realistic one.

### Run it

Use bounded parameters — the defaults are far too large for this section to be
useful interactively:

```sh
go run ./examples/26_social_scale_bench \
  -users 100000 -articles 5000 -friends-min 20 -friends-max 40 -likes-max 60 \
  -profile-dir /tmp/ex26prof
```

> **Timeouts.** A `perl`-style alarm is absorbed by a Go binary. If you need a
> time bound, use a background watchdog instead:
>
> ```sh
> ./ex26 <flags> & APP=$!
> ( sleep 1500; kill -TERM $APP ) & WD=$!
> wait $APP; kill $WD 2>/dev/null
> ```

### Indicators

| Indicator | Meaning |
|---|---|
| `csr.runs_ordered` | the #2141 invariant, asserted live. The run **fails** if false |
| `csr.order` / `csr.size` | vertices and arcs in the snapshot the queries traverse |
| `csr.bytes` / `csr.bytes_per_arc` | snapshot footprint. Three `uint64` arrays, so ~8 B/arc is the floor |
| `csr.has_handles` | whether a handle column exists (always `true`: every edge slot carries a stable identity) |
| `degree.measured` | **which** degree is reported — `FRIEND+LIKE combined`, because a CSR run carries both types and has no type information |
| `degree.expected_range` | the range the generator can produce; the run **fails** if the measurement escapes it |
| `degree.min` / `max` / `mean` / `p50` / `p99` | the realised distribution |
| `degree.threshold` | the **calibrated** crossover, 16 — not the refuted 64 |
| `degree.vertex_frac_above` | share of sources above the threshold |
| `degree.edge_frac_above` | share of arcs leaving them |
| `degree.cost_frac_above` | share of Σd² from them — the linear-scan cost model, and the only one of the three that predicts a speed-up |
| `hub.reverse_expand_rows` | rows from the reverse expand (label-constrained, so identical in both `-rel-types` modes) |
| `# hub.reverse_expand_warm` / `_cold` | the same query with the #2143 pair cache hot and cold |
| `# csr.mem.*` | `runtime.MemStats` deltas across the section |
| `# pprof.cpu` / `# pprof.heap` | profile paths, when `-profile-dir` is set |

### Reading `cost_frac_above=100%` correctly

This is the section's most misreadable line. **100% does not mean the fixture is
hub-heavy.** It means every vertex is the same size and all of them happen to sit
above the crossover. On a power law the three fractions **spread** — `bench/csrorder`
measures 23.57% / 50.09% / 87.22% — and that spread is what skew looks like.

### Why the traversal expands BACKWARDS

The ordering is consumed by the forward-position probes, and they fire on the
**reverse** and undirected expand path (`cypher/exec.Expand.advanceRevEdge`):
each reverse slot must locate its corresponding forward edge, which costs
O(deg(dst)) in the destination's forward run. A purely outward expand walks a
contiguous run and never probes, so measuring one would report the ordering as
pure overhead.

Both a warm and a cold measurement are taken. The cold arm builds a fresh
`Engine`, so it pays the full O(V+E) pair build **and** the ordering pass inside
the measurement; the warm arm reuses the engine and hits the #2143 pair cache.
Measured at the bounded parameters above: **1.78 s cold against 35.8 ms warm, a
49.6× gap** — this example's own view of what that cache is worth. Reporting only
one of the two would misstate the cost by the entire build.

### Sample output

```text
# --- CSR neighbour ordering (sprint 313) ---
# csr.build_elapsed=103ms
csr.order=105000
csr.size=5993585
csr.runs_ordered=true
csr.bytes=46.67 MiB
csr.bytes_per_arc=8.2
csr.has_handles=true
degree.threshold=16
degree.sources=100000
degree.min=20
degree.max=100
degree.mean=59.94
degree.p50=60
degree.p99=96
degree.vertex_frac_above=100.00%
degree.edge_frac_above=100.00%
degree.cost_frac_above=100.00%
degree.measured=FRIEND+LIKE combined (a CSR run carries both types)
degree.expected_range=[20,100]
degree.distribution=BOUNDED, LIGHT-TAILED (not power-law)
hub.reverse_expand_rows=831
# hub.reverse_expand_warm=35.83ms
# hub.reverse_expand_cold=1.777848s
```

Full measurement of the ordering itself, across the degree sweep and against the
pre-sprint tree, is in
[`docs/benchmarks/csr-neighbour-ordering-2026-07-29.md`](../../docs/benchmarks/csr-neighbour-ordering-2026-07-29.md).

## Fused cyclic expand exercise (#2157)

**Scenario.** Mutual-friendship triangles — the natural closed motif in a social
graph. "Who are three people who all follow each other?" is the shape behind
community detection, clique-ish grouping and triadic-closure recommendation.

**Objective.** Contrast the fused cyclic expand against the two-`Expand` plan it
replaces, at **two scales**, so the scaling behaviour is visible in this example's
own output rather than only in a benchmark.

A directed triangle plans today as a chain whose last two hops are an *open* expand
that materialises the whole of `b`'s neighbourhood and a *closing* seek that throws
away all but the closing candidates. Together they compute `N_out(b) ∩ N_in(a)` — by
materialising the left operand in full first. `exec.ExpandIntersect` computes the
same set directly, so a candidate that does not close the cycle is never built into a
row.

**Why the fusing query carries no node labels, and why that matters.** The recogniser
fires only when the cycle's closing hop sits *directly* on its open middle hop. A
node-label predicate interposes a `Selection` between the two, so
`(a:USER)-[:FRIEND]->(b:USER)-…` correctly **declines**. On this dataset `FRIEND`
edges only ever join users, so dropping the labels is semantically equivalent *and*
lets the operator engage. The exercise therefore runs **both** spellings. The
labelled row is not a throwaway control — it is the honest measure of how much of
this win a real label-using query gets today, which is **none**, and it is measured
to be *slower still* because the label predicates are evaluated per intermediate row.

**Fixture.** A uniform ring, deliberately, rather than this example's skewed
population: it is the **honest floor**. SPIKE #2155 proved the two plans' work terms
are *exactly equal* on a regular graph, so the contrast carries no degree-skew
advantage whatsoever.

**Bounded by construction.** This exercise never runs at the `-users` scale. The plan
it is compared against enumerates `Θ(n·d²)` intermediate rows, so the two-`Expand`
arm — not the fused one — is the binding constraint on runtime, and at this example's
default population it would be unbounded.

### Indicators

Deterministic facts (pinned by `TestCyclicJoinExercise`):

| Fact | Meaning |
|---|---|
| `cyclic.nN.triangles` | directed triangles found; exactly `3N` for this ring |
| `cyclic.nN.plans_agree` | the two plans returned the identical count |
| `cyclic.nN.fused_engaged` | the operator actually RAN (see below) |
| `cyclic.nN.twoexpand_engaged` | must be false — the control |
| `cyclic.nN.labelled_declines` | the labelled spelling did not fuse |
| `cyclic.nN.labelled_agrees` | it still returned the same answer |

Volatile telemetry (`# ` prefixed): per-arm latency, `runtime.MemStats` allocation
bytes, and the derived `speedup` / `alloc_ratio`.

**Why an engagement counter and not just the contrast.** SPIKE #2155 verified the
openCypher TCK contains **no directed cycle over three or more distinct node
variables**, so `TCK 3897/3897` cannot see this operator at all. A plan A/B is blind
the same way: if the recogniser silently declined, both arms would run today's plan
and `plans_agree` would be trivially true. Only the counter separates "identical
because it is correct" from "identical because it never fired".

### Observed (Apple M4, `-users=1500`)

| Scale | fused | two-`Expand` | speed-up | alloc ratio | labelled |
|---|---:|---:|---:|---:|---:|
| n=4 000 (12 000 triangles) | 16.8 ms | 64.0 ms | **3.82×** | **7.56×** | 266.9 ms |
| n=12 000 (36 000 triangles) | 54.6 ms | 204.5 ms | **3.74×** | **7.61×** | 833.1 ms |

Both arms scale near-linearly in `n` at fixed degree, with the fused arm a stable
~3.8× ahead — and the **labelled** spelling is roughly 4× slower than *either*,
because it pays label predicates on every intermediate row and cannot fuse. That gap
is the concrete argument for the Selection-hoist follow-on.

pprof CPU and heap profiles are written to `-profile-dir` when set.

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
- `cypher.Engine.RefreshStatistics` — build the planner statistics (HyperLogLog NDV, exact MCV, equi-depth histograms) off the write path in one consistent scan; the single, explicit, caller-driven rebuild.
- `cypher.Engine.Explain` — render the physical plan, each operator annotated with its estimated row count and provenance (`exact` / `heuristic` / `stats` + certified error); the estimates are display-only and never change the executed plan.
- `cypher.Engine.StatsTrackedPairs` — the statistics footprint: the number of `(label, property)` pairs currently tracked (the size indicator the metrics backend cannot express as a gauge).
- `cypher.Result.Next` / `Result.Record` / `Result.Err` / `Result.Close` — iterate result rows and read columns.
- `cypher/expr.StringValue` / `expr.IntegerValue` / `expr.FloatValue` / `expr.ListValue` — typed query parameters and result cells (the list is the `$ids` parameter of the `UNWIND` batch read).
- `runtime.ReadMemStats` — capture the Go heap footprint of the build.

### Cypher language features exercised by the query battery

- **Aggregation** — `count` / `count(*)` / `count(DISTINCT …)`, and (analytical group) `min` / `max` / `avg` / `percentileCont(expr, 0.5)` over a computed friend out-degree distribution.
- **Subqueries** — `EXISTS { (u)-[:LIKE]->(:ARTICLE) }` and its `NOT EXISTS { … }` complement as `WHERE` filters.
- **Projection** — a `CASE WHEN … THEN … ELSE … END` bucketing expression over the degree tertiles.
- **Set operations** — `UNION ALL` combining two label-count streams.
- **Parameters and list unrolling** — `UNWIND $ids AS id MATCH (u:USER {id:id}) …` batch point-read driven by an `expr.ListValue` parameter.
- **Entity identity** — `id()` (Integer) and `elementId()` (String) on a matched node.
- **Temporal functions** — `date($s)` / `datetime($s)` constructors; `duration('P30D')` and `Date − Duration` arithmetic; `duration.inDays(t1, t2)` / `duration.inSeconds(t1, t2)` projections and the `.days` / `.seconds` duration component accessors; and `date.truncate('year', …)` with the `.year` accessor.
- **Planner statistics** — `EXPLAIN` over a label scan, a property equality, and a property range, reading each operator's cardinality estimate and provenance tag from the annotated plan (`RefreshStatistics` + `Explain` + `StatsTrackedPairs`).

## Further reading

- [`graph/lpg`](../../graph/lpg) — labelled property graph package
- [`cypher`](../../cypher) — Cypher engine package
- [Example 22 — Cypher](../22_cypher) — the Cypher engine over a small graph
- [Example 11 — Social network](../11_social_network) — analytics over a small social LPG
- [Example 24 — Social-network CLI](../24_social_network_cli) — a persistent social-network store
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
