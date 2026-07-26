# Columnar shape coverage — measured

**Task #2186** · sprint 320 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='BenchmarkColumnarShape' -benchmem -count=6 -benchtime=10x ./cypher/`

Every figure is the six-run distribution reported by `benchstat`. The benchmarks live in
`cypher/columnar_shape_bench_test.go` and the engagement probe in
`cypher/columnar_shape_coverage_test.go`; both are permanent and are the regression
gates for this task.

---

## 1. One audit premise did not survive re-measurement

The round-3 audit's finding F1b called the **anchor filter** — a `WHERE` on the
*starting* node of a traversal — a *"permanent blind spot"*, on the reasoning that the
canonical plan is `Projection → Expand → Selection → scan` with the `Selection` *below*
the `Expand`, where neither recogniser looks.

Re-measuring refutes it. `Engine.Explain` shows this translator emits the anchor
predicate into the **same** `Selection` slot as a post-traversal one:

```
MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v     ← post-traversal filter
ProduceResults └─ Projection └─ Selection └─ Expand └─ NodeByLabelScan [a:P]

MATCH (a:P)-[:K]->(m) WHERE a.v > 10 RETURN m.v     ← anchor filter: IDENTICAL shape
ProduceResults └─ Projection └─ Selection └─ Expand └─ NodeByLabelScan [a:P]
```

The engagement probe confirms both were already on the columnar path at the audit
baseline, and both cost the same 3.8 ms / 2 183 allocations. So the traversal-group
baseline is **4/7**, not the 3/7 the audit reported, and the acceptance criterion
"the anchor-filter shape is recognised and executes on the columnar path" was already
satisfied before this task.

What the audit measured as a 4.4× cliff is real, but its cause is entirely the
**stacked `Selection`** that a *pattern label* produces, plus the absence of a
conjunction combiner. That is what this task fixes.

## 2. Coverage

Probed with the metrics counter the columnar filter increments per batch, on the
audit's own shape list.

| Group | Audit baseline | After #2186 |
|---|---|---|
| Single scan, filter-and-project | 5/15 | **9/15** |
| Single hop | 4/7 (audit reported 3/7 — see §1) | **7/7** |

Newly admitted: conjunction (same property, different properties, n-way), label test,
`IN` over a scalar literal list, stacked-`Selection` fusion over a traversal, and a
chunk-transparent `LIMIT`.

Still on the row path, with the reason each declines — recorded so the list is a work
queue rather than a silence:

| Shape | Why it declines |
|---|---|
| disjunction (`OR`) | No `OR` combiner. A 3VL `OR` cannot decide a *drop* from one undecided operand, so it needs a different rule from `AND` — not a copy of it. |
| negation (`NOT`) | No `NOT` combiner. `NOT` over an undecided operand is undecided; over a decided *drop* it is not necessarily a keep, because the drop folds FALSE and NULL together. Needs a three-state predicate, not the two-state `(keep, decided)`. |
| `STARTS WITH` | No prefix `ChunkPredicate`. |
| computed projection (`n.v + 1`) | The projection, not the predicate: every item must be a bare property access. |
| `ORDER BY` | `Sort` is a pipeline breaker and not a `ChunkProducer` — the same structural reason `LIMIT` had before this task. |
| `DISTINCT` | As `ORDER BY`. |

## 3. Cost, measured

20 000 nodes / 19 999 edges, every node carrying label `P`, so adding `:P` to a
pattern endpoint **cannot change any result** — which is what makes the labelled arms
exact controls. Each pair below returns an identical multiset.

| Query | before | after | time | allocs |
|---|---|---|---|---|
| `WHERE n.v > 10 RETURN n.v` (control) | 1.682 ms / 127 | 1.680 ms / 127 | ~ (p=0.818) | ~ |
| `WHERE n.v > 10 AND n.v < 19000` | 5.828 ms / 79 354 | **2.110 ms / 148** | **−63.8 %** | **−99.81 %** |
| `WHERE n.v > 10 AND n.w >= 0` | 5.640 ms / 39 615 | **2.246 ms / 146** | **−60.2 %** | **−99.63 %** |
| `WHERE n.w IN [0..6]` | 6.443 ms / 59 890 | **3.180 ms / 186** | **−50.7 %** | **−99.69 %** |
| `(a:P)-[:K]->(m) WHERE m.v > 10` (control) | 3.833 ms / 2 182 | 3.860 ms / 2 185 | ~ (p=0.937) | +0.14 % |
| `(a:P)-[:K]->(m:P) WHERE m.v > 10` | 13.738 ms / 120 890 | **4.264 ms / 2 202** | **−69.0 %** | **−98.18 %** |
| `(a:P)-[:K]->(m) WHERE a.v > 10` (control) | 3.821 ms / 2 183 | 3.867 ms / 2 185 | ~ (p=0.937) | +0.11 % |
| `(a:P)-[:K]->(m:P) WHERE a.v > 10` | 14.279 ms / 120 889 | **4.314 ms / 2 202** | **−69.8 %** | **−98.18 %** |
| `(a:P)-[:K]->(m) WHERE a.v > 10 AND m.v < 19000` | 10.222 ms / 160 622 | **4.190 ms / 2 211** | **−59.0 %** | **−98.62 %** |
| `WHERE n.v > 10 RETURN n.v` (control) | 1.641 ms / 127 | 1.704 ms / 127 | ~ (p=0.240) | ~ |
| `WHERE n.v > 10 RETURN n.v LIMIT 20000` | 7.956 ms / 59 366 | **1.659 ms / 137** | **−79.2 %** | **−99.77 %** |
| **geomean (11 arms)** | 5.490 ms / 13 140 | **2.798 ms / 497** | **−49.0 %** | **−96.2 %** |

Bytes: geomean 1.872 MiB → 1.252 MiB, **−33.1 %**.

**The cliff is gone, not merely reduced.** The labelled far-endpoint arm now costs
4.264 ms against the unlabelled control's 3.860 ms — within run-to-run noise, where it
was 3.6× before. The inert `LIMIT` arm costs 1.659 ms against the no-`LIMIT` control's
1.704 ms; it was 4.8× before.

Every control arm is statistically unchanged (p ≥ 0.240 on time), which is what
attributes the whole effect to the newly admitted shapes rather than to drift.

## 4. Correctness

`cypher/columnar_shape_identity_test.go` runs each newly admitted shape twice — once as
written, once with every property access wrapped in `coalesce(x)`, an exact value
identity both the columnar predicate and the columnar projection decline, so that arm
executes fully boxed — and requires an identical result multiset. A metrics probe
asserts the columnar filter engaged on the first arm and did **not** on the second, so
no case can pass vacuously.

The fixture is deliberately hostile: the compared property spans `int64` (including
the extremes and values outside Go's small-int box range), `float64` (including −0.0,
NaN and both infinities), strings (including the empty string and a SOH-tagged string
that is *not* a valid temporal), booleans, a real temporal, and an **absent** property.
That forces every branch of the predicate — the same-kind unboxed compare, the
cross-type kind mismatch that must report *undecided*, the temporal-tagged string that
must report *undecided*, and the absent property that is a decided drop under
three-valued logic.

`cypher/columnar_limit_identity_test.go` sweeps every `LIMIT` from 0 through both chunk
capacity boundaries (4095/4096/4097) to past the whole result, asserting the exact row
count and the exact ordered prefix against the row-mode reference, plus the
`SKIP`-below-`LIMIT` composition and the shared-counter invariant when the same
operator is drained row-at-a-time.

Test power was verified by mutation. Four seeded defects — ignoring an undecided
conjunct, a label test that always passes, an `IN` that never falls back, and both an
off-by-one and a dropped accumulator in the `LIMIT` clamp — were each caught by these
suites.

TCK: 3897 scenarios, 3897 passed, 0 failed, 0 undefined.

## 5. Two rewrites the columnar path must yield to

Widening the predicate grammar to accept a label test made the single-scan recogniser
claim `MATCH (n:A:B)`, which is exactly the shape the min-cardinality label re-anchor
(#2077) exists for; and fusing stacked `Selection`s made the traversal recogniser claim
the shape the anchor swap exists for. Both of those rewrites reduce **cardinality**,
while columnar execution removes only a **constant factor**, so both recognisers now
decline when the higher-value rewrite would fire. Their differential suites
(`min_label_scan_diff_test.go`, `anchor_swap_diff_test.go`) caught this, and are what
pins it.

One expectation genuinely changed. `TestParallelScanProject_Differential/filter-and`
asserted that a pure-projection conjunction engages the *parallel* fused scan, which it
did only because the columnar chain could not handle a conjunction. It now can, and the
columnar chain is the documented preference for that shape — measured here at 2.110 ms /
148 allocations against the parallel path's 5.828 ms / 79 354. That case now carries the
same `n.v + 0` item its sibling cases already carried, for the same stated reason: to
keep that test measuring the parallel path.
