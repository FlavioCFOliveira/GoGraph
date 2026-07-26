# Bound-key index seek — measured

**Task #2184** · sprint 319 · 2026-07-26 · Apple M4 · `go test -bench=. -benchmem -benchtime=50x -count=6 ./bench/cypher_boundkey/...`

Every figure is the median of six runs. The benchmark lives in
`bench/cypher_boundkey/` and is permanent: it is the regression gate for tasks
#2182 and #2183.

The "before" column throughout is the #2181 spike's measurement of the same query
shape on the same machine, recorded in `docs/design-correlated-seek.md` §2. It is
independently corroborated below (§4).

---

## 1. A single bound key — task #2182

`WITH 'name-1' AS k MATCH (a:P {name: k}) RETURN a`, against the inline literal
control.

| Label population | inline literal | `WITH`-bound, before | `WITH`-bound, after | `WITH $p`-bound, after |
|---|---|---|---|---|
| 5 000 | 4.18 µs | 2.72 ms | **8.41 µs** | 8.21 µs |
| 10 000 | 3.89 µs | 5.54 ms | **7.97 µs** | 8.16 µs |
| 20 000 | 4.04 µs | 13.37 ms | **8.38 µs** | 8.38 µs |

The bound form is now **flat in the label population**, as the inline form already
was. At N = 20 000 that is **1 595×**.

Allocations: 43/op inline, 106/op bound. The residual ~2× against the inline case —
8.38 µs versus 4.04 µs — is the `Apply` and `Projection` layers plus the retained
filter, inherent to the shape rather than a defect in the access path.

A parameter binding seeks as well, and costs the same as a literal binding. That
matters because the plan is cached by query text: the rewrite moves the
`*ast.Parameter` node rather than its value, so one cached plan stays correct for
every invocation's parameters.

---

## 2. A key set — task #2183

`UNWIND [<k keys>] AS k MATCH (a:P {name: k}) RETURN a` at N = 20 000. The cost
gate's ceiling is 10 % of the label population, so 2 000 unique keys sit exactly at
the budget and 2 001 is one key past it.

| Keys | Access path | Before | After | Allocs/op | Gain |
|---|---|---|---|---|---|
| 1 | single-key seek | 11.79 ms | **7.70 µs** | 107 | **1 531×** |
| 30 | key-set seek | 12.59 ms | **51.63 µs** | 821 | **244×** |
| 300 | key-set seek | 12.25 ms | **601.61 µs** | 7 537 | **20×** |
| 2 000 | key-set seek (at the budget) | ~15 ms | **8.35 ms** | 53 400 | ~1.8× |
| 2 001 | **scan — gate declined** | 15.75 ms | 15.05 ms | 263 522 | 1× by design |

The gain shrinks with key count exactly as the spike predicted, because it is
N/rows rather than N per row, and the gate switches the rewrite off one key before
it would invert.

---

## 3. The declined gate does not regress — acceptance criterion (3)

The 2 001-key case was measured on the pre-#2183 code (commit `8bb6553`, in a
detached worktree) with the identical benchmark file, so this is the same query and
the same fixture on both sides:

| 2 001 keys, N = 20 000 | ns/op | allocs/op |
|---|---|---|
| before (`8bb6553`) | 15 749 961 | 263 519 |
| after | 15 049 350 | **263 522** |

Allocations are identical to within three, and the timing difference is within
run-to-run noise. That is the strongest form the criterion can take: the declined
plan is not merely *close to* the pre-rewrite plan, it allocates the same.

Reaching that took two fixes, both found by measuring rather than reasoning:

1. **An unclaimed hint was being evaluated.** The rewrite pushes the key set into
   the `Apply`'s inner arm as a disjunction so the seek can recognise it. When the
   gate declined, that disjunction stayed and cost Θ(k·N) predicate evaluations —
   **2 952 ms** and 76 109 847 allocations at 2 001 keys. The build now drops an
   unclaimed hint, and EXPLAIN renders the dropped shape.

2. **The key set was materialised before being rejected.** Extraction boxes one
   `expr.Value` per disjunct and builds a deduplication map over them, on every
   build — that is, once per execution. Rejecting *after* paying that cost left
   **20.2 ms** and 6 021 surplus allocations, almost exactly 3 per key. A size gate
   now declines before extraction, which is §3.3's third condition in the design
   document and makes the plan-time cost of a rejected set O(1).

A gate that declines into a plan slower than the one it was meant to preserve is
not a gate. Both regressions were on the *fallback* path, which is precisely where
a cost gate's correctness lives.

---

## 4. What is NOT served, and what that does to the audit's attribution

The round-3 audit attributed its headline load deficit — **35 m 33 s** to load
20 000 nodes and 200 000 edges, against Memgraph's 977 ms and Neo4j's 2.39 s — to a
bound key never reaching the index. Measured, that attribution does not hold.

| 30 keys, N = 20 000 | Access path | ns/op | allocs/op |
|---|---|---|---|
| literal list `UNWIND ['name-1', …]` | key-set seek | **51 630** | 821 |
| runtime list `UNWIND $keys` | scan | 12 588 010 | 240 070 |
| row property `UNWIND $rows AS r … {name: r.name}` | scan | 12 653 170 | 240 130 |

The third row is the audit's own load query
(`bench/comparison/threeway_test.go:430`):

```cypher
UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts})
CREATE (a)-[:KNOWS]->(b)
```

Its key is a **property access on the unwound row**, not a bare bound variable, and
its list is a **runtime parameter**, not a literal. Neither #2182 nor #2183 changes
its plan: it still plans a full label scan per endpoint — two scans and a nested
`CartesianProduct` — verified through `Engine.Explain`. So the load figure is
**unchanged** by this sprint's work, and the audit's attribution was wrong about the
mechanism even though it was right that a bound key never reached the index.

Serving that shape needs an operator that drains its input before probing: a barrier
over the input rather than a leaf, with its own cost profile and cancellation story.
The load deficit itself is addressed instead by **`gograph-import`** (#2180), which
loads a store from CSV in 0.28 s.

### 4.1 The "before" figures are corroborated

The runtime-list path at 30 keys costs 12.59 ms today. The spike measured the
*pre-change* bound-key path at 30 keys at 12.59 ms. Those are different code paths
measured at different times, and they agree to three significant figures — which is
expected, since both are "scan the label once and join the keys against it", and
which independently corroborates the before-column of every table above.

---

## 5. The benchmark guards its own labelling

Each benchmark name embeds the access path its query actually gets — `seek`,
`seekset`, or `scan` — read from `Engine.Explain` at registration time. A planner
change that silently moved a case onto a different path would otherwise report
timings under a name that no longer described them, which is worse than a failure.
`TestAccessPathsAreWhatTheBenchmarkNamesClaim` asserts every expected path, so the
labelling has a gate of its own.

That test caught one defect in this benchmark during development: the path helper
originally called `Explain` with nil parameters, which resolves an absent parameter
to NULL and therefore declines a seek — labelling the seeking `WITH $p`-bound case
"scan" while timing it correctly. The helper now takes the parameters the query will
actually run with.

---

## 6. Reproducing

```bash
go test -bench=. -benchmem -benchtime=50x -count=6 ./bench/cypher_boundkey/...
```

Use an **entity** projection for any further work here. The spike measured the
inline-literal seek at 204/415/895 µs across N = 5k/10k/20k on a *scalar*
projection — growing linearly despite the plan showing `NodeByIndexSeek` — because
the columnar scan-and-filter path claims a scalar-projecting `Selection` and never
consults the seek (**#2204**). A scalar-projection benchmark measures #2204 twice
and this work not at all.
