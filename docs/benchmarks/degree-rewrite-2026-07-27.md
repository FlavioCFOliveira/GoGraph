# Degree rewrites for COUNT and EXISTS — 2026-07-27 (rmp #2232)

Part 2 of the degree work. Part 1 ([`degree-primitive-2026-07-27.md`](degree-primitive-2026-07-27.md),
rmp #2218) exposed `OutDegree` / `OutDegreeByType` on the adjacency; this record covers the
planner-side rewrite that puts them to use, and — as importantly — the eligibility boundary that
turned out to be narrower than the audit assumed.

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harness `bench/r4audit/degree_test.go`, build tag
  `r4audit`. Fixture: N nodes labelled `:P`, out-degree 4, relationships typed `:K`.
- All figures are microseconds **per outer row** at N=8000, best of 5, against a bare
  `MATCH (a:P) RETURN count(a)` baseline of 0.028 µs.

## 1. The eligibility boundary, and why AC 1 was re-targeted

The task's acceptance criterion named `COUNT { (a)-[:K]->(:P) } > 0` — measured by the round-4
audit at 88× the bare scan — as the figure to move. Reading the reference design showed that shape
is **not degree-rewritable, in Neo4j either**.

Neo4j 5.26's `getDegreeRewriter.scala` (pinned SHA `eccd584a64d468af3daeab421478fe78567c518f`)
gates on `QuerySolvableByGetDegree`, whose pattern match requires — among `SetExtractor()` on
almost every other query-graph field — **`Selections.empty`**. A label on a pattern node is a
Selection (`HasLabels`) in Neo4j's IR. So a labelled far node disqualifies the pattern. The reason
is not an oversight: a degree counts *every* out-edge of a node and has no way to filter on what
the far node is.

The criterion was therefore re-targeted, with the user's decision, onto the shapes a faithful port
*can* admit. The labelled shape is filed as **rmp #2235**, explicitly as a *different* mechanism —
a single filtered adjacency walk, Θ(d) not Θ(1) — and explicitly **not** as a widening of this
recogniser.

## 2. Result

| shape | before | after | vs baseline | change |
|---|---|---|---|---|
| `COUNT { (a)-[:K]->() } > 0` | 1.605 µs | **0.487 µs** | 58.1× → **17.2×** | **3.4× faster** |
| `COUNT { (a)-[:K]->() } = 4` | 1.624 µs | **0.477 µs** | 58.8× → **16.8×** | **3.5× faster** |
| `COUNT { (a)-->() } > 0` | 0.929 µs | **0.453 µs** | 33.6× → **16.0×** | **2.1× faster** |
| `EXISTS { (a)-[:K]->() }` | 0.243 µs | 0.251 µs | 8.8× → 8.9× | unchanged (control) |
| `size([ (a)-[:K]->(b) \| b ])` in RETURN | 2.275 µs | 2.143 µs | 82.4× → 75.5× | unchanged (control) |
| `COUNT { (a)-[:K]->(:P) } > 0` | 2.282 µs | 2.257 µs | 82.6× → 79.5× | unchanged — ineligible by design |

Two of the unchanged rows are **expected** to be unchanged, and are kept as controls rather than
quietly dropped:

- **`EXISTS { … }` never reaches this rewrite.** A top-level EXISTS is lowered to a `SemiApply` in
  the IR (`cypher/ir/exists.go`) before the expression evaluator sees it, which is exactly why it
  was already the cheapest of the measured shapes at 8.8×. The rewrite still covers EXISTS in the
  positions where it survives as an expression.
- **`size([pattern | …])` in a RETURN projection is hoisted** into a `RollUpApply` for the same
  reason. The rewrite covers `size()` where the comprehension survives as an expression — in a
  `WHERE`, for instance — which `TestDegreeRewrite_Identity/size(pattern_comprehension)_in_a_predicate`
  pins.

## 3. Where the remaining 17× is

Not in the degree lookup. Measured on the same fixture at N=8000:

| | µs/row | delta |
|---|---|---|
| bare scan | 0.0279 | — |
| scan + `WHERE 1 > 0` | 0.1331 | **+0.105 — the per-row predicate machinery itself** |
| scan + `WHERE a.id >= 0` | 0.2094 | +0.076 over the predicate floor |
| scan + `WHERE COUNT { (a)-[:K]->() } > 0` | 0.4954 | +0.362 over the predicate floor |

Simply *having* a WHERE predicate costs 3.8× the bare scan before any predicate work happens. What
remains above that floor is the expression evaluator's per-evaluation overhead — several
string-keyed `RowContext` lookups, including the one that smuggles the subquery context — not the
adjacency access. Reducing it is a change to the expression evaluator, not to this rewrite.

## 4. A hypothesis of mine that measurement mostly refuted

I predicted the dominant remaining cost was the node-id → node-key → node-id round-trip (an array
read plus a string hash) forced by the value-keyed degree API, and added id-keyed entry points
(`AdjList.OutDegreeByID`, `Graph.OutDegreeByID`, and the bounded companions) to remove it.

Measured effect: **0.5663 → 0.4954 µs/row, about 12%.** Real, reproducible and worth keeping —
the query layer holds ids and this is the right primitive for it — but nowhere near the dominant
term I expected. The dominant term is the predicate machinery in §3. Recorded here rather than
quietly presented as the win it was not.

## 5. Short-circuiting

`COUNT { … } <op> <literal>` is evaluated with a ceiling of `literal+1`, which is provably enough
to decide all six comparison operators (the proof is in the `expr.BoundedCountEvaluator` doc
comment). The untyped degree needs no ceiling — it is one column length, O(1). The typed degree
does, since it must read the label column, and `OutDegreeByTypeBoundedByID` stops as soon as the
cap is reached.

`TestDegreeRewrite_ShortCircuits` proves the walk really stops rather than the result merely being
clamped: on a degree-20 000 hub with a cap of 3, the per-slot predicate is invoked at most 3 times.

## 6. The defect the differential suite nearly missed

An inverted operand-order condition in `evalBoundedCountComparison` swapped the two sides of every
`COUNT { … } <op> <literal>`, so every such predicate selected nothing. **The differential suite
was green**, because both the rewritten form and its control go through that same comparison code
— a defect there breaks both arms identically.

It was caught only by comparing against a baseline build. The suite now also asserts a
hand-computed **absolute** expected value for every case, which is the only oracle that does not
share the code under test; re-introducing the inversion now fails it immediately. This is the
second time in this sprint that "two forms agree" turned out to be a weaker claim than it looks.

## Reproduce

```bash
go test -tags=r4audit -run 'TestPerOuterRowCost_DegreeEligible' -v -timeout 30m ./bench/r4audit/
go test -run 'TestDegreeRewrite' ./cypher/
```
