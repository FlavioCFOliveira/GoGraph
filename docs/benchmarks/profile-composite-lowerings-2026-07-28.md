# Instrumenting composite lowerings so every PROFILE operator is measured — 2026-07-28 (rmp #2237)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Fixture: 300 `:P` nodes, 40 distinct ages
  (`seedPeople(t, 300, 40)`).
- Allocation evidence from `bench/cypher_alloc`, `-count=5 -benchmem`, compared with `benchstat`.
- "before" is the tree at `9936d03d`.

## 1. The gap

`buildOperator` applies the profiler at exactly ONE point — the value its recursion returns — which
covers the whole physical tree exactly once, at no cost when no profiler is installed. That works
while one logical node lowers to one physical operator. Where the lowering is COMPOSITE it does not:
only the outermost operator passes through the wrap point, and the intermediates beneath it rendered
`(not measured)`.

Five operators were affected, across the five shapes the profiling gates run over:

| shape | unmeasured before |
|---|---|
| `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)` | `EagerAggregation`, `ColumnarProject` |
| `MATCH (n:P) WHERE n.age > 10 RETURN n.name` | `ColumnarFilter` |
| `MATCH (n:P) RETURN count(n)` | `EagerAggregation`, `ColumnarProject` |
| `MATCH (n:P) RETURN n.age ORDER BY n.age LIMIT 5` | `Project` (the plan ROOT) |

Before and after, on the join-under-aggregation shape:

```
Project (rows=1, time=549µs)                             Project (rows=1, time=436µs)
└─ GlobalAggregateAdapter (rows=1, time=549µs)           └─ GlobalAggregateAdapter (rows=1, time=436µs)
   └─ EagerAggregation (not measured)          ───▶         └─ EagerAggregation (rows=1, time=436µs)
      └─ ColumnarProject (not measured)                        └─ ColumnarProject (rows=2260, time=412µs)
         └─ ColumnarHashJoin (rows=2260, …)                       └─ ColumnarHashJoin (rows=2260, …)
```

**An unmeasured node is worse than an absent one.** It is still rendered, so a reader counts it as
part of the plan — while its cost silently lands in whichever ancestor *was* measured. In the shape
above, all of the grouping and pre-projection cost appeared to belong to `GlobalAggregateAdapter`,
an operator whose only job is to synthesise one row for an empty input.

The `Project` case is the sharpest: it is the plan ROOT, built by `wrapWithColumnPassthrough` which
sits ABOVE `buildOperator`'s recursion entirely. No amount of care inside the recursion could have
reached it.

## 2. The change

One helper, `profileIntermediate(bopts, op)`, applied at each composite site: both aggregation
pre-projections (columnar and row-mode), the `EagerAggregation` beneath a `GlobalAggregateAdapter`,
both `ColumnarFilter` sites beneath a `ColumnarProject`, and the final result projection — which
required threading `bopts` into `wrapWithColumnPassthrough`.

Three properties make it safe to apply at any site:

- **Cost when off is zero.** With no profiler installed the helper returns `op` untouched: no wrapper
  is allocated and no operator executes instrumentation. This is the property #2237 required be kept.
- **Double-wrapping is safe.** `exec.Profiler.Wrap` is idempotent, so a site that wraps an
  intermediate which later also passes the single wrap point cannot double-count.
- **Capabilities survive.** `Wrap` re-implements `ChunkProducer` and `NodeIDColumnProducer`. This is
  load-bearing *here* in a way it is not at the single wrap point: an intermediate is wrapped BEFORE
  its parent is constructed, so the parent's capability type-assertions run against the wrapper. The
  columnar aggregation pre-projection is exactly that case — `EagerAggregation.WithChunkInput` must
  still recognise it, or profiling would silently downgrade a columnar plan to row mode.

The public `BuildPlan` is deliberately left uninstrumented: it never carries a profiler, since
PROFILE goes through `buildPlanEngine`.

## 3. No allocation regression

`bench/cypher_alloc`, five runs each, `benchstat`:

| benchmark | allocs/op before → after | B/op before → after | sec/op |
|---|---|---|---|
| AllNodesScan | 514 → 514 | 12.06 KiB → 12.06 KiB | ~ (p=0.056) |
| FilterOp | 515 → 515 | 12.11 KiB → 12.11 KiB | ~ (p=0.222) |
| ProjectOp | 517 → 517 | 12.25 KiB → 12.25 KiB | ~ (p=0.460) |
| ResultSet | 518 → 518 | 12.35 KiB → 12.35 KiB | ~ (p=0.095) |
| geomean | +0.00% | +0.00% | +0.40% |

Every allocation figure is byte-identical — `benchstat` reports "all samples are equal" — and no
timing difference is significant at p < 0.05.

**A caveat on what that does and does not prove.** `bench/cypher_alloc` constructs its operators BY
HAND rather than through the planner, so it does not execute the lowering sites this change touches;
the identical numbers establish that nothing global moved, not that the new call sites are free. What
establishes *that* is structural: `profileIntermediate` returns its argument on a nil profiler, which
is every non-PROFILE query, so the added work outside PROFILE is one nil comparison per composite
site per plan build. Recorded rather than glossed, because a benchmark that cannot see a change is
weak evidence and should not be presented as strong.

## 4. Correctness

`TestProfile_EveryOperatorIsMeasured` asserts no node renders `(not measured)` across the five
shapes, and additionally that measurements are present at all — so a shape that stopped producing a
tree could not pass the first check vacuously. It fails on the pre-change tree for four of the five
shapes.

`TestProfile_PlanShapeIsIdenticalProfiledOrNot` continues to compare the profiled and unprofiled
renderings node for node, which is what would catch a wrapper that hid a capability. Both gates now
iterate one shared `profileShapes` list, so they cannot drift onto different plans — the two
properties (every node measured; the shape unchanged by measuring it) have to hold over the same
shapes to mean anything together.

## 5. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`, cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**.

## Reproduce

```bash
go test -count=1 -run 'TestProfile' -v ./cypher/
go test -count=5 -bench=. -benchmem -run '^$' ./bench/cypher_alloc/
```
