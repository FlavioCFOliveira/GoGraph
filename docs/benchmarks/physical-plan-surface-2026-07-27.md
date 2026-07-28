# A faithful physical plan, and PROFILE — 2026-07-27 (rmp #2222)

- Apple M4 (10 cores, 32 GB), Go 1.26.5, darwin/arm64.
- Allocation gate: `bench/cypher_alloc`, `benchstat` over `-count=6` (and a confirmation run at
  `-count=8`).

## 1. The defect, reproduced before anything was changed

`Engine.Explain` rendered the **logical IR** and re-derived the planner's physical decisions a
second time against it. That reconstruction was wrong in both directions:

- round 3 found it reporting `NodeByIndexSeek` where a label scan ran;
- round 4 found the reverse — for `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)` it
  printed `CartesianProduct` while the runtime counter proved a hash join executed.

Measured on 400 `:P` nodes over 50 age buckets, before any change:

```
hashJoinBuildCount fired = 1
Explain says HashJoin = false | CartesianProduct = true
```

A reader diagnosing that query was shown **O(n·m) where O(n+m) runs**.

## 2. What the surface shows now

Same query, same graph:

```
Project
└─ Project
   └─ GlobalAggregateAdapter
      └─ EagerAggregation
         └─ ColumnarProject
            └─ ColumnarHashJoin [build=right]
               ├─ NodeByLabelScan [P]
               └─ NodeByLabelScan [P]
```

`Explain` now walks the operator tree the builder actually produced and names each node from its
**concrete Go type**. That is the property the whole surface turns on: a `HashJoin` is named
`HashJoin` because it *is* one, not because a second reconstruction happened to agree. The
inversion is not merely fixed, it is **unrepresentable**.

It also revealed what the old rendering hid entirely: this query runs on the **columnar tier**
(`ColumnarHashJoin`, `ColumnarProject`), which round 3 recorded as invisible.

`PROFILE` adds per-operator measurements from a real execution:

```
Project (not measured)
└─ Project (rows=1, time=857µs)
   └─ GlobalAggregateAdapter (rows=1, time=857µs)
      └─ EagerAggregation (not measured)
         └─ ColumnarProject (not measured)
            └─ ColumnarHashJoin (rows=3200, time=699µs)
               ├─ NodeByLabelScan (rows=400, time=3µs)
               └─ NodeByLabelScan (rows=400, time=3µs)
```

Times are inclusive of children, as in Neo4j's PROFILE; subtract a node's children for its
exclusive cost.

## 3. How drift was made impossible rather than merely fixed

Three mechanisms, in the order they matter:

1. **One build path.** `Engine.buildReadPhysical` resolves every optimisation gate — hash join and
   its order-safety companion, range seeks, min-label scan, disjoint-component reorder,
   single-edge anchor swap, parallel tier, memory budget — and both `Run` and the rendering
   surfaces call it. There is no second place for a rendering to re-derive anything.
2. **Names from the operators themselves**, via `reflect` on the concrete type.
3. **A source-derived completeness gate.** An operator's inputs live in unexported fields, which
   reflection cannot read, so structure comes from an explicit `PlanChildren` method — and a
   missing method would silently *truncate* the rendered plan, hiding everything beneath it.
   `TestPlanChildren_EveryOperatorWithInputsImplementsIt` parses the package, treats every struct
   with a `Next(out *Row)` method as an operator (resolving embedded promotion), and requires
   `PlanChildren` on any that holds a field whose type implements `Operator`. It found **46**
   such operators, four of them columnar ones a regex over field types had missed.

## 4. Allocation and time gate (AC 3)

Two independent runs. `allocs/op` and `B/op` are **exactly identical** in both — every case, all
samples equal:

| | run 1 (`-count=6`) | run 2 (`-count=8`) |
|---|---|---|
| `allocs/op` | +0.00% (all samples equal) | +0.00% (all samples equal) |
| `B/op` | +0.00% (all samples equal) | +0.00% (all samples equal) |
| `sec/op` geomean | +1.20% | **−0.88%** |
| `AllNodesScan` | +2.12% (p=0.002) | **−1.12% (p=0.021)** |

The first run suggested a ~2% time cost, which would have been the extra call frame added per plan
node. **It did not reproduce, and the sign flipped**: these micro-benchmarks are sensitive to code
layout, and there is no reliable time regression. Both runs are reported rather than the
favourable one, because reporting only run 2 would have been the same kind of untruth this task
existed to remove.

Zero allocation cost is structural, not lucky: profiling is a wrapper installed by the builder at
one point and only when a `Profiler` is present, so an ordinary `Run` executes code identical to a
build in which profiling does not exist.

## 5. Why the wrapper had to live in `cypher/exec`

`cypher/explain` already had a `ProfiledOperator` recording rows and elapsed time, written before
any engine surface existed to wire it to. It could not be used, for a structural reason: the
builder wraps on the way **out** of its recursion, so a parent is constructed with its child
already wrapped and runs its capability type-assertions against the wrapper. A wrapper must
therefore re-implement `NodeIDColumnProducer`, whose identifying method `nodeIDColumnProducer()`
is **unexported to `exec`** — no wrapper declared elsewhere can satisfy it. Wrapping from
`cypher/explain` would have stripped the marker and silently downgraded a columnar plan to row
mode, so every measurement would have described a plan the user never runs.
`TestProfile_PlanShapeIsIdenticalProfiledOrNot` compares the two renderings node for node and is
the gate on that.

## 6. What this surface then exposed

Rendering the truth immediately produced a defect the old rendering could not show:
`MATCH (n:Person) RETURN n` builds **two stacked `Project` operators** for one `RETURN`, while the
logical plan has a single `Projection` — every row is projected twice on the engine's hottest
path. Filed as **#2239**.

## 7. Scope boundaries, stated

- **Writing statements** render the logical plan and say so on the first line. A write's physical
  tree binds to an open transaction, so there is none to walk outside one; the label is preferable
  to silently returning a different kind of plan.
- **Cardinality estimates** live on logical nodes and have no counterpart on a built operator, so
  they remain available through the new `Engine.ExplainLogical` rather than being lost.
- **Some operators are unmeasured** in a PROFILE, and are labelled `(not measured)` rather than
  left bare — a bare node would read as one that cost nothing. A composite lowering emits several
  operators for one logical node and only the outermost passes the wrap point. Filed as **#2237**.
- **db-hits** are not collected: they happen inside an operator, not at its boundary, so a wrapper
  cannot count them. Filed as **#2238**.
- **EXPLAIN/PROFILE query prefixes** are not in scope; both surfaces are Go APIs, so a Bolt client
  cannot yet reach them.

## 8. Reproduction

```bash
go test ./cypher/ -run 'TestExplainPhysical|TestProfile_'
go test ./cypher/exec/ -run TestPlanChildren_Every
go test ./bench/cypher_alloc/ -run '^$' -bench . -benchmem -count=6
```
