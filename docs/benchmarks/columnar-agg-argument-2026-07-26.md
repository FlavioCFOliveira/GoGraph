# Columnar aggregate argument — measured

**Task #2185** · sprint 320 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='BenchmarkColumnarAggArg' -benchmem -count=6 -benchtime=5x ./cypher/`

Every figure is the six-run distribution reported by `benchstat`. The benchmark lives
in `cypher/agg_columnar_arg_bench_test.go` and is permanent: it is the regression
gate for this task, alongside the allocation gate in
`cypher/agg_columnar_argument_alloc_test.go`.

---

## 1. What was wrong

The columnar aggregation pre-projection (`tryBuildColumnarAggInput`, `cypher/api.go`)
filled its **grouping key** through `buildScalarPropertyFiller`, which reads the
property straight off the raw NodeID and lands a plain scalar in the chunk's typed
column with no heap box. It filled every **aggregate argument** through
`evalPutColumnFiller`, which calls the row-at-a-time evaluator and boxes the result.

It also declined outright when there was no grouping key, so **every global aggregate
was fully row-mode**.

The round-3 audit measured the consequence and attributed it precisely: the whole
allocation step between `count(*)` (no argument) and `min(n.v)` (a property argument)
was the argument filler — see `docs/audit-2026-07-26-streams/s05-runtime.md` F4.

## 2. The measurement

100 000 nodes, each carrying `v` (the aggregate argument, `int64`) and `w` (the
grouping key, 7 distinct values). `DisableParallelScan` is set on every arm so the
serial columnar `EagerAggregation` is what is measured — the parallel aggregate scan
is a different physical path and a different task (#2187).

Allocations per input row = `allocs/op ÷ 100 000`.

| Query | allocs/op before | per row | allocs/op after | per row |
|---|---|---|---|---|
| `RETURN n.w, count(*)` (no argument — the floor) | 99.95k | 1.00 | 99.95k | 1.00 |
| `RETURN n.w, min(n.v)` | 699.72k | 7.00 | **99.97k** | **1.00** |
| `RETURN n.w, max(n.v)` | 699.73k | 7.00 | **99.97k** | **1.00** |
| `RETURN n.w, sum(n.v)` | 699.73k | 7.00 | **99.97k** | **1.00** |
| `RETURN n.w, avg(n.v)` | 699.73k | 7.00 | **99.97k** | **1.00** |
| `RETURN min(n.v)` (global) | 699.64k | 7.00 | **99.91k** | **1.00** |
| `RETURN sum(n.v)` (global) | 699.64k | 7.00 | **99.91k** | **1.00** |
| `RETURN avg(n.v)` (global) | 699.64k | 7.00 | **99.90k** | **1.00** |

**A property argument now costs exactly what `count(*)` costs.** The acceptance
criterion was that it *approach* the `count(*)` floor; it reaches it. The residual
1.00 allocation per row is the scan's own NodeID box, which belongs to task #2188,
not here.

## 3. benchstat A/B

```
                             │  before   │             after              │
                             │  sec/op   │   sec/op     vs base           │
ColumnarAggArg_GroupCount-10    10.94m ± 13%   11.73m ±  8%        ~ (p=0.180 n=6)
ColumnarAggArg_GroupMin-10      47.05m ±  2%   15.74m ± 20%  -66.54% (p=0.002 n=6)
ColumnarAggArg_GroupMax-10      46.56m ±  5%   15.07m ± 13%  -67.62% (p=0.002 n=6)
ColumnarAggArg_GroupSum-10      46.63m ±  1%   13.51m ± 15%  -71.01% (p=0.002 n=6)
ColumnarAggArg_GroupAvg-10      45.93m ±  3%   13.86m ± 10%  -69.82% (p=0.002 n=6)
ColumnarAggArg_GlobalMin-10    43.391m ±  2%   9.902m ± 12%  -77.18% (p=0.002 n=6)
ColumnarAggArg_GlobalSum-10    42.010m ±  4%   9.765m ± 21%  -76.76% (p=0.002 n=6)
ColumnarAggArg_GlobalAvg-10    42.061m ±  2%   9.870m ± 10%  -76.53% (p=0.002 n=6)
geomean                         37.53m         12.22m        -67.43%

                             │   B/op    │      B/op      vs base         │
ColumnarAggArg_GroupCount-10   4.848Mi ± 0%   4.848Mi ± 0%        ~ (p=0.892 n=6)
ColumnarAggArg_GroupMin-10    74.391Mi ± 0%   4.959Mi ± 0%  -93.33% (p=0.002 n=6)
ColumnarAggArg_GroupSum-10    74.391Mi ± 0%   4.959Mi ± 0%  -93.33% (p=0.002 n=6)
ColumnarAggArg_GlobalMin-10   74.111Mi ± 0%   4.834Mi ± 0%  -93.48% (p=0.002 n=6)
geomean                        52.80Mi        4.898Mi       -90.72%

                             │ allocs/op │   allocs/op    vs base         │
ColumnarAggArg_GroupCount-10    99.95k ± 0%   99.95k ± 0%        ~ (p=0.697 n=6)
ColumnarAggArg_GroupMin-10     699.72k ± 0%   99.97k ± 0%  -85.71% (p=0.002 n=6)
ColumnarAggArg_GlobalMin-10    699.64k ± 0%   99.91k ± 0%  -85.72% (p=0.002 n=6)
geomean                         548.6k        99.94k       -81.78%
```

Summary, over the six arms that changed path:

| Dimension | Before | After | Change |
|---|---|---|---|
| Time (geomean, all 8 arms) | 37.53 ms | 12.22 ms | **−67.4 %** |
| Bytes (geomean, all 8 arms) | 52.80 MiB | 4.898 MiB | **−90.7 %** |
| Allocations (geomean, all 8 arms) | 548.6k | 99.94k | **−81.8 %** |

The `GroupCount` arm has no aggregate argument, so it is the control: it shows no
change (p = 0.180 on time, p = 0.697 on allocations), confirming the effect is the
argument filler and nothing else.

The global arms gain more than the grouped arms (−77 % versus −67 %) because they
were previously declined *entirely*, so they also pick up the unboxed key-free group
assignment, not just the unboxed argument.

## 4. Why the numbers are trustworthy

The differential correctness suite (`cypher/agg_columnar_argument_test.go`) runs every
one of eleven property kinds — `int64`, `float64`, mixed int/float, `bool`, `string`,
absent (NULL), temporal, date, list, bytes, heterogeneous — against every one of
eleven aggregates — `min`, `max`, `sum`, `avg`, `count`, `collect`, `stDev`,
`percentileCont`, and the `DISTINCT` variants of `count`/`sum`/`collect` — both
grouped and global, and asserts the result is identical to the same query with the
argument wrapped in `coalesce(x, x)`, which the builder declines and which therefore
executes fully row-mode. That is 242 differential pairs, plus exact-`int64`-`SUM`
(CIP2016-06-14), empty-input neutral rows, cross-batch type change across the 4096-row
chunk boundary, a multi-aggregate projection, and the relationship-property argument
that the node-only guard correctly declines.

A differential test passes vacuously if both arms end up on the same path, so
`cypher/agg_columnar_argument_alloc_test.go` (`!race`) pins engagement directly: the
columnar arm must stay under 3 allocations per input row and the boxed control must
stay above it. Measured on 20 000 rows: **0.99** versus **14.97** (grouped) and
**7.98** (global).

TCK: 3897 scenarios, 3897 passed, 0 failed, 0 undefined.
