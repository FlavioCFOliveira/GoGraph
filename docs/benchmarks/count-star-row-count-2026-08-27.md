# Removing the `count(*)` pre-projection — measured

**Task:** rmp #2625 · **Date:** 2026-08-27 · **Machine:** Apple M4, darwin 25.5.0

## What changed

A group-by-less, non-DISTINCT `count(*)` has a **constant** aggregate argument:
`aggArgItem` emits `expr.BoolValue(true)` for an empty argument, so `CountAgg`
ticks on every row and can never reject one. The serial pipeline nevertheless
built a pre-projection that materialised **one fresh single-column row per input
row** to carry that constant.

`exec.CountRows` counts the child's rows instead. It is tried **after** every
existing count pushdown, because those answer in O(1) from a maintained counter
while this still visits every row — it only stops *building* rows nobody reads.

`count(v)` is deliberately excluded: it counts non-null **bindings**, so its
argument must still be evaluated per row.

## The plan

```
before                                after
Project                               Project
└─ GlobalAggregateAdapter             └─ CountRows
   └─ EagerAggregation                   └─ Filter
      └─ Project      <- 1 row/edge         └─ Expand
         └─ Expand                             └─ NodeByLabelScan [USER]
            └─ NodeByLabelScan [USER]
```

## The gain depends entirely on the child

Two shapes, same 80 000 edges, profile times inclusive:

| shape | pre-projection / `CountRows` own | dominant operator |
|---|---|---|
| `()-[:FRIEND]->()` | 4.06 ms of 11.30 ms (**~36%**) | itself |
| `(:USER)-[:FRIEND]->(:USER)` | ~2.6 ms of 26.1 ms (**~10%**) | `Filter` ~18.2 ms (**~70%**) |

End-to-end on the bare shape: **4.52 ms → 2.71 ms (~40%)**.

## At scale: a modest gain, a large stability gain

`examples/26_social_scale_bench -users 40000 -articles 4000 -friends-min 150
-friends-max 200 -likes-max 300 -seed 1`, **interleaved** A/B, five runs per arm,
two prebuilt binaries alternated:

| metric | before (median) | after (median) | delta | before spread | after spread |
|---|---|---|---|---|---|
| `count_friend` (6 997 085 edges) | 8.040 s — 1.149 µs/edge | 7.504 s — 1.073 µs/edge | **+6.7%** | **52%** | **4%** |
| `count_like` (5 967 899 edges) | 6.331 s — 1.061 µs/edge | 6.156 s — 1.031 µs/edge | **+2.8%** | **59%** | **9%** |

Min-vs-min: +4.8% and +3.9%.

**The variance is the headline, not the median.** Removing seven million per-row
allocations takes the GC out of the query's tail: the before arm's fastest and
slowest runs differ by 52–59%, the after arm's by 4–9%.

A single early pairing showed 35%. Five interleaved rounds identify it as an
outlier in the *before* arm; it is not claimed anywhere.

## What this did NOT fix

**The dominant cost of the filed query is the per-row `Filter` checking the far
endpoint's `:USER` label — about 70%.** The expansion already knows the
relationship type, but nothing pushes the destination label into it, so all seven
million rows pay a separate predicate. Filed separately.

## Refuted along the way

The task prescribed serving the count from `graph/index/count` as Neo4j does.
`count.Store.CountE(rt)` takes **no snapshot** and its cells are plain
`atomic.Int64` with no version chain, so it reports only the CURRENT live count.
A read transaction pins its view: inside `BeginReadTx` the scan answered **2000**
both before and after 100 further edges committed outside it, while a fresh query
answered **2100**. Answering from the store would have returned 2100 inside that
transaction — a snapshot-isolation violation. Neo4j can do this because its
counts store is transactional and versioned with the log; GoGraph's is a derived
planner statistic, consulted today only by `cypher/count_estimate.go`.
