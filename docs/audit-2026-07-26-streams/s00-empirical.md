# Stream 0 — Empirical three-way head-to-head (GoGraph vs Neo4j vs Memgraph)

This stream closes the evidence gap recorded on 2026-07-24: *"No empirical head-to-head vs
Neo4j/Memgraph exists (all verdicts architectural)."* It is the first time the three systems have
been measured against each other on this project.

## Method

- Harness: `bench/comparison/threeway_test.go`, build tag `threeway`.
- Four targets, so transport cost is separated from engine cost:
  - `gograph-embedded` — in-process `cypher.Engine`, no serialisation (GoGraph's real mode)
  - `gograph-bolt` — GoGraph behind its own Bolt server, driven by the official `neo4j-go-driver/v5`
  - `neo4j-bolt` — Neo4j 5.26 Community, Docker, 2 GB heap + 2 GB page cache
  - `memgraph-bolt` — Memgraph 2.22.0, Docker, defaults
- Host: Apple M4, 10 cores, 32 GB. Containers on colima (6 CPU / 14 GB VM).
- Identical deterministic dataset (PCG seed 31,1) loaded into every target through identical
  `UNWIND` batches; identical Cypher for every query except two documented dialect divergences
  (index DDL syntax; Memgraph's `*BFS` in place of `shortestPath()`, which it does not implement).
- Median of 9 after 3 warm-ups, with an adaptive budget for queries slower than 50 ms/500 ms/5 s.
- Result-row counts are cross-checked across all four targets to prove the queries are semantically
  equivalent before any timing is compared.

**Fairness caveats, stated up front.** Neo4j and Memgraph run in a VM behind a TCP round trip;
`gograph-embedded` does not. That is why `gograph-bolt` exists — it is the apples-to-apples column.
Neo4j Community lacks the Enterprise pipelined/Morsel runtime, so these numbers do not represent
Neo4j's best. Conversely GoGraph is being run in the mode it is actually designed for.

> **CORRECTION (post-hoc, from Stream 6's independent verification).** F0.1 below is right that
> numeric *equality* full-scans, and right about the cause in the hash index. It is **wrong** in two
> details, and Stream 6's diagnosis supersedes it:
>
> 1. Numeric **range** predicates DO reach an index. `a.id >= 12345 AND a.id < 12346` plans as
>    `NodeByIndexRangeScan (rows=2, exact)`, served by a hidden float64 btree companion
>    (`cypher/api.go:2394`). My measurement showed a full scan because my probe graph had only a
>    hash index available. Only numeric **equality** degenerates.
> 2. Because that float64 companion already ships, the fix is **cheaper than I proposed**: do NOT
>    build a new float64 hash index as `cypher/api.go:9710` suggests — instead rewrite numeric `=`
>    into the closed range `[v, v]` on the companion that already exists. Effort **S**, not M.
>
> Stream 6 also found the deeper and more general root cause of the load collapse, which F0.1
> under-diagnosed: `resolveSeekValue` (`cypher/api.go:9642`) takes the seek value as raw **source
> text** and resolves only `$param` and literals. Any key bound by a preceding row — `UNWIND` **or**
> `WITH` — therefore loses the index entirely, regardless of type. Even
> `WITH 'name-1' AS nm MATCH (a:P {name: nm})` plans `CartesianProduct(Projection, NodeByLabelScan)`.
> There is no per-row seek operator at all. Measured: seek path flat in N (1.96 → 1.84 ms as N goes
> 5k → 20k) versus the UNWIND path growing (5.81 → 11.80 ms) — Θ(rows·N) against Θ(rows).
>
> The corrected causal chain for the 2 184× load deficit is therefore: **correlated seek keys never
> reach any index** (primary), with **numeric equality never reaching one either** (secondary and
> independent). Both are recorded in the final report under their corrected form.

## F0.1 — Cypher `CREATE INDEX` cannot index numeric properties  [NEW] (severity: HIGH)

**The finding.** GoGraph's Cypher DDL can only build **string-keyed** indexes. Every numeric
predicate — inline map, `WHERE` equality, `WHERE` range — falls back to `NodeByLabelScan`.

Measured on 20 000 `:Person` nodes with indexes on both an integer `id` and a string `name`
(`Engine.Explain`, so this is the plan, not an inference):

| Predicate form | Access path chosen |
|---|---|
| `MATCH (a:Person {id: $x})` (int, param) | `NodeByLabelScan` — FULL SCAN |
| `MATCH (a:Person {id: 12345})` (int, literal) | `NodeByLabelScan` — FULL SCAN |
| `MATCH (a:Person) WHERE a.id = $x` (int) | `NodeByLabelScan` — FULL SCAN |
| `MATCH (a:Person) WHERE a.id >= 12345 AND a.id < 12346` (int) | `NodeByLabelScan` — FULL SCAN |
| `MATCH (a:Person) WHERE a.name = $s` (string) | **`NodeByIndexSeek`** |
| `MATCH (a:Person {name: $s})` (string) | **`NodeByIndexSeek`** |

This is not a planner oversight; it is an explicit, documented contract:

- `cypher/index_binding.go:55` `projectStringPropValue` returns `ok=false` for any
  `pv.Kind() != lpg.PropString`, so a numeric property is **never inserted into the index at all**.
- `cypher/api.go:9705` states the contract directly: *"an int64 hash index is a Go-API-only building
  block — a Cypher CREATE INDEX never builds one (hash indexes are string-keyed)"*.
- `cypher/range_seek_plan.go:24` says the same for the btree: *"The index is a TYPED string btree
  (the only btree a Cypher CREATE INDEX can build)… Integer/float btrees are NOT created by Cypher."*

**Why it was done this way, and why that reasoning is sound but incomplete.** The comment at
`cypher/api.go:9705` explains the hazard precisely: openCypher numeric equality is **cross-type**
(`5 = 5.0` is TRUE), so an `int64`-keyed hash index would silently drop a float-valued node that is
equal to an integer seek key. Declining the seek was the correct *conservative* choice — it keeps
results spec-faithful. The gap is that the conservative choice was never followed by the sound one.

**The consequences, measured.**

1. Bulk edge loading joins on an integer key in essentially every real dataset. Loading
   2 000 nodes + 19 931 edges through identical `UNWIND` batches:

   | Target | Load time | vs GoGraph |
   |---|---|---|
   | gograph-embedded | **19.31 s** | — |
   | neo4j-bolt | 1.85 s | **10.4× faster** |
   | memgraph-bolt | **0.127 s** | **152× faster** |

2. Decomposing that cost (`bench/comparison/probe_load_test.go`):

   | Operation | Cost |
   |---|---|
   | Node creation via Cypher | 3.7 µs/node |
   | Edge creation via Cypher, **with** an index on the int join key | 939.4 µs/edge |
   | Edge creation via Cypher, **without** any index | 940.1 µs/edge |
   | Edge creation via the **native Go API** (`g.AddEdge`) | **0.28 µs/edge** |

   The index changes nothing (939.4 vs 940.1 µs) — proof it is not consulted. The Cypher write path
   is **3 354×** slower than the native API for the same logical work.

3. Cost grows exactly linearly in label population, which is the signature of a full scan
   (`bench/comparison/probe_writeindex_test.go`, 500 edges, index present on the int key):

   | `:Person` count | µs/edge |
   |---|---|
   | 1 000 | 448.7 |
   | 2 000 | 902.9 |
   | 4 000 | 1 888.9 |
   | 8 000 | 3 784.9 |

   Doubling the node count doubles the per-edge cost. O(N) per row, not O(log N).

**What Neo4j and Memgraph do.** Both index numeric properties natively. Neo4j 5 RANGE indexes cover
all orderable property types (INTEGER, FLOAT, STRING, temporal, spatial, and arrays thereof), and
Neo4j resolves the int/float comparability question inside the index by ordering all numbers in one
numeric domain. Memgraph's label-property index likewise covers numeric properties.

**The lever — and it is already specified in GoGraph's own source.** `cypher/api.go:9710` prescribes
the fix: *"key it on float64 and keep a residual filter (as the range companion does)."* That is
exactly right and is the pattern the string range seek already proves correct:

- Index all numeric values in a single **float64-ordered domain**, so `5` and `5.0` collide as
  openCypher requires.
- Return a **superset** and always retain the residual filter, which is precisely the discipline
  `range_seek_plan.go` already documents and enforces.
- Precedent already exists in-tree: aggregation grouping already uses float64-domain bucketing with
  an exact comparator (`cmpInt64Float64`), so the exact int/float comparison machinery is built and
  tested.

**TCK/ACID impact.** TCK-neutral: the residual filter is always retained, so the result set is
identical — the seek only narrows the candidate set. Round 1 established there are no index/constraint
TCK feature files, so index behaviour is not directly TCK-covered; correctness is preserved through
the superset+filter invariant, not through the index. ACID-neutral: index maintenance already runs
inside the existing commit barrier for string indexes; extending the projected key type does not add
a new write path. Effort: **M**.

**Ranking.** This is the highest-value single change surfaced by this audit. It converts the most
common access pattern in graph workloads (integer-keyed lookup) from O(N) to O(log N), and it is the
direct cause of a measured 10×/152× bulk-load deficit against the two incumbents.

## F0.2 — Cyclic patterns: ~80× slower than both incumbents  [NEW, strengthens R2-F3] (severity: HIGH)

Triangle count `MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN count(*)`
on 2 000 nodes / 19 931 edges:

| Target | Median |
|---|---|
| gograph-embedded | 2.164 s |
| gograph-bolt | 1.703 s |
| neo4j-bolt | **26.3 ms** |
| memgraph-bolt | **20.0 ms** |

GoGraph is **82× slower than Neo4j** and **108× slower than Memgraph** on the same query, same data,
same returned answer.

`Engine.Explain` shows why:

```
EagerAggregation
└─ Selection
   └─ Expand (c)-[:KNOWS]->(__anon_3_to_a)      ← closing edge
      └─ Selection
         └─ Expand (b)-[:KNOWS]->(c)
            └─ Selection
               └─ Expand (a)-[:KNOWS]->(b) (est. rows=9927, exact)
                  └─ NodeByLabelScan [a:Person] (est. rows=1000, exact)
```

The closing edge `(c)-[:KNOWS]->(a)` has **both endpoints already bound**, yet it is planned as a
full `Expand` into a fresh synthetic variable `__anon_3_to_a` followed by a `Selection` equating it
to `a`. Instead of asking "does edge c→a exist?" (one probe), the engine enumerates every
out-neighbour of `c` and filters. Cost per 2-path is Θ(deg(c)) instead of Θ(1)/Θ(log d).

**This confirms round-2 finding 3 (`ExpandInto` absent, `cypher/ir/match.go:1348-1363`) and shows it
matters far more than the round-2 synthetic measurement implied** — because the bound-destination
case is not an edge case, it is what every cyclic pattern reduces to.

**It also reframes round 2's WCOJ conclusion.** Round 2 positioned worst-case-optimal joins as *"the
one place GoGraph could be asymptotically better than both"* incumbents. That remains theoretically
true (both are binary-join-only, so neither meets the AGM bound), but the measured present-day
reality is the opposite: GoGraph is ~80× **worse** on exactly this shape. The correct sequencing is
therefore to fix `ExpandInto` **first** — it is a bounded, well-understood change that closes most of
an 80× gap — and treat WCOJ as the later, more speculative step. Round 2 scheduled `ExpandInto` as
sprint 314, behind the keystone sprint 313; this evidence argues it is the most acute measured
deficit in the engine and should be re-prioritised on its own merits.

**TCK/ACID impact.** `ExpandInto` is a pure access-path substitution under an unchanged result
contract, and the existing `Selection` can be retained as a residual during rollout. TCK-neutral,
ACID-neutral. Effort: **M** (the round-2 analysis notes the probe is cheapest over sorted adjacency,
which is the keystone sprint-313 work — but a hash-set probe over the bound node's adjacency is
available without waiting for it).

## F0.3 — Stale defect claim in test documentation  [NEW] (severity: LOW)

`bench/ldbc/ic_helpers_test.go:12-15` (and repeated at lines ~45-49) states:

> *"inline integer property filters in a comma-joined MATCH pattern (MATCH (a:P {id:1}),(b:P {id:2}))
> are silently no-op in the current engine when both patterns carry integer filters"*

and the LDBC seed queries are written in a `WHERE`-clause workaround form because of it.

**This is no longer true.** `bench/comparison/probe_inline_test.go` exercises all four variants
(inline int both sides, `WHERE` form, inline string both sides, and the read-only path); every one
returns the correct single row/edge. The comment documents a defect that has since been fixed.

CLAUDE.md requires documentation to be *"accurate and faithful to the code"*. A prominent comment
asserting a silent-wrong-results bug that does not exist is a documentation defect: it will cause
future work to route around a non-problem, and it undermines trust in the surrounding commentary.
**Lever:** correct the comment and simplify the seed queries to the natural inline form.
Effort: **S**.

## Notes on the harness itself

`bench/comparison/threeway_test.go` is a reusable deliverable, not a throwaway: it is build-tagged
(`threeway`) so it never runs in `make ci`, it is parameterised by `THREEWAY_NODES`, and it emits a
Markdown report via `THREEWAY_OUT`. It cross-checks returned row counts across all four targets
before comparing any timing, so a dialect divergence cannot silently masquerade as a speed
difference. Recommend promoting it into the repo permanently and recording its output under
`docs/benchmarks/`, which is where CLAUDE.md requires comparative numbers to live.

One harness-level observation worth recording: `cypher.BindParams` accepts `[]any` and
`map[string]any` but not typed slices such as `[]map[string]any`, which the official Neo4j driver
accepts without ceremony. It is documented behaviour rather than a bug, but it is an avoidable
ergonomic difference for anyone porting driver code to the embedded API. Effort to widen: **S**.
