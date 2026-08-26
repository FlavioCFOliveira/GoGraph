# The parallel-scan tier stopped pre-empting the min-cardinality label anchor

**Task:** rmp #2431 · **Date:** 2026-08-26 · **Host:** Apple M4, 10 cores, darwin/arm64, go1.26.5
**Tree:** `feature/350-bugs-solving`, fix in `cypher/api.go` (`tryBuildParallelScanProject`)

## Summary

A multi-label pattern whose first label is large and whose second is small —
`MATCH (n:Common:Rare) RETURN n.k` over 100 000 `:Common` of which 1 000 also carry `:Rare` —
planned a morsel-parallel scan of `:Common` instead of the serial re-anchored scan of `:Rare`.
The default configuration was **25.5× slower** than the plan the same engine already knew how
to build, and slower than the legacy full-`:Common` scan the re-anchor was written to replace.

The fix makes the parallel gate judge the anchor the serial path would choose, and then anchor
**on it**, rather than declining to parallelise. No measured shape regresses; two improve.

## Root cause

`tryBuildColumnarFilterChain` already yields to the re-anchor and states the rule:

> The min-cardinality label re-anchor (#2077) replaces the scanned label with the smallest one
> in the conjunction, which reduces the number of rows SCANNED. Columnar execution only removes
> a constant factor from each scanned row, so it must never pre-empt the re-anchor.

The identical argument applies to the morsel-parallel scan, which divides the cost of each
scanned row by the worker count — also a constant factor — and it was not making it.

The consequence was worse than a missed optimisation. The columnar chain is tried **first** and
declines exactly the `(n:A:B)` shapes the re-anchor exists for; `tryBuildParallelScanProject`
was tried immediately after and claimed every one of them, anchored on `Labels[0]`. **The yield
was inert**: it handed the shape straight to an operator that ignored the same rule.

### The measurement that proved which label the gate was judging

Holding `|Rare|` at 1 000 and sweeping `|Common|`, best of 20:

| `\|Common\|` | best | plan |
|---:|---:|---|
| 100 000 | 4.611 ms | `Project / ParallelScanProject` |
| 50 001 | 2.217 ms | `Project / ParallelScanProject` |
| 50 000 | 0.197 ms | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| 40 000 | 0.197 ms | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |

The flip is exactly at `|Common| > 50 000`, the parallel threshold, while the re-anchored label
never moves. The gate was judging the **first** label, not the anchor.

## Option sweep on the reported fixture, best of 30

| configuration | before | after | plan after |
|---|---:|---:|---|
| default | 4.611 ms | **0.196 ms** | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| `DisableParallelScan` | 0.186 ms | 0.198 ms | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| `DisableBitmapIntersection` | 5.575 ms | 0.190 ms | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| `DisableMinLabelScan` | 4.280 ms | 3.287 ms | `ColumnarProject / ColumnarFilter / NodeByLabelScan [Common]` |

All arms return exactly 1 000 rows, asserted every time. The bitmap intersection is confirmed
not implicated: before the fix, disabling it changed neither plan nor time.

## Interleaved A/B, 5 pairs, `benchstat`

The arms are interleaved rather than run back to back, so a drift in machine state cannot be
read as an effect. `p` values are from `benchstat`; `n=5`.

| benchmark | sec/op | B/op | allocs/op |
|---|---:|---:|---:|
| **`P2431_Fixture`** (100 000 / 1 000) | **−96.08%** (p=0.008) | −99.28% | **−98.54%** (106 293 → 1 551) |
| `P2431_MinLabelAboveThreshold` (200 000 / 120 000) | −19.92% (p=0.008) | −11.95% | −14.65% |
| `P2431_MinLabelAboveThresholdSkewed` (200 000 / 60 000) | −44.16% (p=0.008) | −33.47% | −37.56% |
| `P2431_SingleLabel` (200 000, one label) | ~ (p=0.151) | ~ (p=1.000) | ~ (p=1.000) |

**No shape regresses.** The two shapes where the parallel scan legitimately wins get *faster*,
because the gate now anchors the parallel scan on the smaller label instead of declining to use
the anchor at all. The single-label shape — which has no conjunction to re-anchor — is
untouched on all three metrics.

## The named benchmarks, which now assert what their names claim

| benchmark | before | after |
|---|---:|---:|
| `BenchmarkMinLabelScanSelective_MinLabel` | 4 980 µs (§6 of the release-delta: 5 519 µs) | **204.7 µs** |
| `BenchmarkMinLabelScanSelective_FirstLabel` | 3 400 µs | 3 421 µs |

`_MinLabel` is now **16.6× faster** than `_FirstLabel`, which is the relationship its name
asserts and which did not hold before. It is also faster than v0.10.0's 230.3 µs on the same
fixture, so the regression is not merely undone.

## The regression gate

`cypher/parallel_scan_min_label_precedence_test.go` pins the precedence **structurally** — it
compares plans, never wall-clock. A timing gate for this would have to separate a 25× planning
difference from machine load, and this sprint is largely a cleanup of gates that could not
(#2517, #2572, #2589). It fails on the pre-fix tree and passes after.

Every arm carries its own non-vacuity control, because the way this gate would rot is for the
fixture to stop being eligible for the parallel path, at which point "the plan is not parallel"
becomes true for the wrong reason.

**One control had to be replaced, and the replacement is the point.** The obvious control —
disable the re-anchor and check the fixture still plans a parallel scan — *fails*, because
disabling the re-anchor also unblocks the columnar chain, which then claims the shape and the
parallel tier is never reached. The gate therefore drives `RETURN n.k + 1`, which is not a plain
property access: the columnar chain declines it at every setting, so the parallel tier is the
only operator left deciding. The reported `RETURN n.k` shape is asserted as well, end to end.

## Scope left open, deliberately

When the re-anchored label is itself above the threshold the parallel scan now anchors on it,
which is a strict improvement. It still applies the full `LabelPredicate` per row rather than
only the residual labels, so the chosen label is re-checked redundantly. That costs a boolean
per row against a cardinality reduction of the whole scan, and removing it would mean teaching
the per-worker factory to rebuild the residual predicate; it is not attempted here.

## Reproduce

```
go test ./cypher/ -run 'TestParallelScanYieldsToMinLabelAnchor|TestParallelScanStillWins|TestSingleLabelScanIsUnaffected' -count=1
go test ./bench/cypher_ldbc/ -run='^$' -bench=BenchmarkMinLabelScanSelective -benchmem -count=5
```
