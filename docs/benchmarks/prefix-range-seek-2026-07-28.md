# `STARTS WITH` served from the sorted btree — 2026-07-28 (rmp #2127, measured under #2129)

- Apple M4 (10 cores, 32 GB), `darwin/arm64`, Go 1.26.5.
- Harness `bench/cypher_ldbc/prefix_seek_bench_test.go`, committed with this change.
- Fixture as specified by the round-2 planner audit
  (`docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.1): 50 000 `:PfxBench`
  nodes, `name = "name%05d"`, bound btree index on `name` built through
  `CREATE INDEX`; the prefix `"name002"` selects **100 rows — 0.2%** of the
  population, well inside the seek's 10% gate.
- `benchstat` over `-count=10 -benchmem`.
- `TestPrefixSeekBenchRowCountsAgree` asserts all four variants return the same
  100 rows **before** any timing is believed, so the benchmark compares like with
  like rather than a fast wrong answer against a slow right one.

## 1. Result

Four arms on one fixture: the plan the rewrite replaces (`labelscan` — the same
prefix predicate with the rewrite disabled via
`EngineOptions.DisablePrefixIndexSeek`), the rewritten plan (`indexseek`), the
hand-written range equivalent `>= "name002" AND < "name003"` (`rangeequiv`, the
cost the audit named as the target), and that range form plus a third conjunct
(`rangeequiv3`, see §3).

```
              │   labelscan    │              indexseek              │             rangeequiv              │              rangeequiv3              │
              │     sec/op     │   sec/op     vs base                │   sec/op     vs base                │    sec/op      vs base                │
PrefixSeek-10   11812.61µ ± 1%   30.67µ ± 1%  -99.74% (p=0.000 n=10)   38.63µ ± 1%  -99.67% (p=0.000 n=10)   3566.12µ ± 2%  -69.81% (p=0.000 n=10)

              │   labelscan    │              indexseek               │              rangeequiv              │             rangeequiv3              │
              │      B/op      │     B/op      vs base                │     B/op      vs base                │     B/op      vs base                │
PrefixSeek-10   1977.16Ki ± 0%   21.79Ki ± 0%  -98.90% (p=0.000 n=10)   24.90Ki ± 0%  -98.74% (p=0.000 n=10)   82.67Ki ± 0%  -95.82% (p=0.000 n=10)

              │   labelscan   │             indexseek              │             rangeequiv             │            rangeequiv3             │
              │   allocs/op   │ allocs/op   vs base                │ allocs/op   vs base                │ allocs/op   vs base                │
PrefixSeek-10   149829.0 ± 0%   368.0 ± 0%  -99.75% (p=0.000 n=10)   566.0 ± 0%  -99.62% (p=0.000 n=10)   104.0 ± 0%  -99.93% (p=0.000 n=10)
```

| Comparison | sec/op | B/op | allocs/op |
|---|--:|--:|--:|
| label scan → prefix seek | **385.2× faster** | **90.7× less** | **407.1× fewer** |
| prefix seek vs range equivalent | **1.26× faster** | 1.14× less | **1.54× fewer** |

**The allocation count is the proof that the label is no longer walked.** Before,
149 829 allocs/op over a 50 000-node label — almost exactly 3 per node, the
signature of a full scan with a per-row property read. After, 368, and it no
longer tracks the population at all.

## 2. Against the audit's prediction

| Source | scan | range/prefix | ratio (time) | ratio (allocs) |
|---|--:|--:|--:|--:|
| Audit §2.1, 2026-07-25 (uncommitted harness) | 11.56 ms | 40.89 µs | 283× | 275× |
| Premise re-verified on the current tree, #2126 | 11.28 ms | 38.23 µs | 295× | 265× |
| This benchmark, prefix seek | 11.81 ms | **30.67 µs** | **385×** | **407×** |

The audit's premise held — it was re-measured before any code was written, since
sprints 326 and 327 had landed in between — and the delivered win exceeds it.

## 3. The prefix seek beats its own range equivalent

The audit framed the goal as "`STARTS WITH` should cost what its range-equivalent
already costs". It costs **less**: 30.67 µs against 38.63 µs, and 368 allocs
against 566.

The candidate explanation was the residual `Filter` rather than the seek: both
plans descend the same btree and both retain the original predicate above the
scan, but the retained predicates differ in size — the prefix form re-checks
**one** `STARTS WITH`, the range form **two** comparisons joined by an `AND`.

That is a hypothesis, so it was tested by adding a third, non-narrowing conjunct
(`AND p.name <> "zzz"`) — same 100 rows, and, it was assumed, the same access
path, which would isolate the cost of one extra residual conjunct.

**The test was invalid and the measurement said so.** Predicted ≈764 allocs;
measured 104, with time up 92×:

| residual predicate | plan | sec/op | allocs/op |
|---|---|--:|--:|
| `STARTS WITH "name002"` | `Filter` → `NodeByIndexRangeScan` | 30.67 µs | 368 |
| `>= "name002" AND < "name003"` | `Filter` → `NodeByIndexRangeScan` | 38.63 µs | 566 |
| `… AND p.name <> "zzz"` | `ColumnarFilter` → `NodeByLabelScan` | **3566.12 µs** | 104 |

Plans confirmed with `Engine.Explain`, not inferred from the numbers.
`extractStringRangePred` recognises only an **exact two-way `AND`**, so the third
conjunct makes it decline and the seek is lost; the resulting all-comparison
predicate is then `ColumnarFilter`-eligible, which is why the arm is
allocation-*light* (104) while still being O(N) in time. Three plans, not one plan
with three residual sizes.

So the mechanism behind the 30.67 µs vs 38.63 µs gap **remains unestablished** and
is recorded as such. What is established is the measured fact: on this fixture the
prefix form is 1.26× faster and needs 1.54× fewer allocations than the range form
it is meant to match, so expressing the intent as a prefix is now at least as good
as hand-rolling the range — the opposite of the advice a user needed before.

Two follow-ups fell out of this and are **out of scope here**, filed rather than
fixed:

- **The three-conjunct cliff** (rmp #2245): one extra conjunct on a range
  predicate gives up the index seek — 3566 µs where the two-conjunct form runs
  38.63 µs, **92×**. `BenchmarkPrefixSeek_RangeEquivalent3` is retained as its
  regression witness.
- **`STARTS WITH` is not `ColumnarFilter`-eligible** (rmp #2246): when the
  selectivity gate declines a prefix — a broad prefix, or the empty prefix — the
  plan falls back to a **row-mode** `Filter` at 11.8 ms / 149 829 allocs, where the
  columnar filter over the same label scan costs 3.57 ms / 104 allocs. That is
  roughly 3.3× left on the table for exactly the cases this sprint's gate
  deliberately declines.

## 4. No regression where the shape is absent

The rewrite must be **inert** on any query without a prefix predicate. This is not
free by inspection: LEDGER row 0023 records a real regression caused by nothing
more than an added struct *field* pushing two per-statement heap structs across a
malloc size class. This change adds a `bool` to both `Engine` and `buildOpts`, so
the curated suite was measured A/B across the exact commit boundary — a worktree
at `927df2e5` (the design-doc-only commit, code identical to `main`) against
`d7cdd9ac` — rather than compared against the previous history run, which would
have carried all of sprints 326 and 327 with it.

The verdict is **inert**, and the decisive evidence is the deterministic columns.
Full delta in
[`history/0027__prefix-range-seek-2127__…delta.txt`](history/0027__prefix-range-seek-2127__d7cdd9ac-dirty.delta.txt).

| group | allocs/op | B/op | sec/op |
|---|---|---|---|
| `bench/cypher_ldbc` (15 benchmarks) | **all 15 identical**, geomean +0.00% | geomean −0.06% | geomean −1.73% |
| `bench/cypher_alloc` (3 benchmarks) | **all 3 identical**, geomean +0.00% | **all 3 identical**, +0.00% | geomean −2.46% |
| `search` (3 benchmarks) | identical | identical | geomean +1.65% |
| `search/centrality` (2 benchmarks) | identical | identical | geomean +1.96% |

**Allocations and bytes are byte-identical on every one of the 23 benchmarks.**
That is the column that would have moved had a struct crossed a malloc size class
— it is precisely how the 0023 regression manifested (`B/op +0.90%`, allocs flat) —
and it did not move here.

`sec/op` drifts in **both directions** by comparable magnitudes: the Cypher groups
read ≈2% faster, the graph-algorithm groups ≈2% slower. Neither is attributable to
this change, and the graph-algorithm side proves it: `search` and
`search/centrality` do not import the Cypher engine at all, so a field added to
`cypher.Engine` cannot reach them. The two runs are ~30 minutes apart on the same
laptop, so ≈2% cross-run drift is the expected noise floor for wall-clock on this
machine. The deterministic counters, which do not drift, are identical — so the
rewrite is inert where its shape is absent, as required.

## 5. Evidence discipline

- The benchmark and its row-count gate are **committed**, so the measurement is
  reproducible rather than asserted: `go test -run='^$' -bench=BenchmarkPrefixSeek
  -benchmem -count=10 ./bench/cypher_ldbc/...`.
- The fixture builds its graph as `Directed + Multigraph`. That is the openCypher
  storage model, and it also stops the engine emitting its non-directed /
  non-multigraph warnings, which would interleave with the benchmark output and
  make `benchstat` silently drop the affected samples.
- Correctness for this change is gated separately and independently, in
  `cypher/prefix_seek_differential_test.go`: an ON-vs-OFF differential, an
  absolute Go oracle over the fixture, plan assertions proving the two arms differ,
  and a `rapid` property — with the harness itself validated by injecting three
  mutations and confirming each one fails. TCK 3897/3897 throughout.
- Design, superset proof and scope boundaries: `docs/design-prefix-range-seek.md`.
