# Partitioned label scan — measured

**Task #2187** · sprint 320 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='BenchmarkParallelLabelScan' -benchmem -cpu=1,10 -count=6 -benchtime=10x ./cypher/`

200 000 nodes, **every one carrying label `P`**, so a labelled query and its unlabelled
twin return the identical multiset and the only difference is which leaf the planner
could parallelise. Benchmarks in `cypher/parallel_label_scan_bench_test.go`.

---

## 1. What was wrong

Every intra-query parallel leaf required the IR leaf to be a bare, unlabelled
`*ir.AllNodesScan`. Real Cypher almost always carries a label, so parallelism never
engaged in practice — the round-3 audit measured the cost of adding a label that every
node already carried at 2.0×–3.7× and confirmed causality by toggling
`DisableParallelScan`. Both incumbents partition the label scan (Neo4j's
`PartitionedNodeByLabelScan`, Memgraph's `ScanParallelByLabel`).

## 2. What changed

A labelled leaf now walks that label's roaring bitmap (`lpgLabelWalker`) and the existing
morsel splitter partitions those ids. No new operator: the parallel leaves already
consume any `nodeWalker`, and the per-worker sub-plan already scans its morsel, which for
a labelled leaf contains only that label's members.

The threshold is keyed on the **label's own cardinality**, not the graph's live order, so
a ten-node label inside a million-node graph stays serial.

Row identity is exact rather than merely equivalent: the walker iterates the same bitmap
from the same resolver a serial `NodeByLabelScan` iterates, and the label index is
already live-only (it strips deleted nodes at delete time).

## 3. The measurement, and the decision it forced

Two leaves were candidates. They disagreed, so only one was admitted.

### Projection leaf — admitted (1.90× at 10 cores)

`MATCH (n:P) RETURN n.v + 0 AS v`, 200 000 rows:

| Arm | 1 core | 10 cores | allocs/op |
|---|---|---|---|
| labelled, parallel | 89.1 ms | **28.5 ms** | 607 787 |
| labelled, serial | 60.9 ms | 57.8 ms | 599 317 |
| unlabelled, parallel (control) | 86.3 ms | 28.1 ms | 607 759 |
| unlabelled, serial (control) | 64.9 ms | 57.9 ms | 599 329 |

At 10 cores the labelled query goes from 57.8 ms to **28.5 ms**, and it now lands within
1.4 % of its unlabelled twin (28.5 vs 28.1 ms) — i.e. the label no longer costs anything.

The labelled and unlabelled arms track each other at **both** core counts, which is the
point: the change extends existing behaviour to labelled scans without introducing any
new pathology.

### Aggregate leaf — NOT admitted (1.6× LOSS)

`MATCH (n:P) RETURN min(n.v) AS m`, 200 000 rows, measured with the labelled leaf
provisionally admitted:

| Arm | 1 core | 10 cores | allocs/op |
|---|---|---|---|
| labelled, parallel | 147.9 ms | 33.9 ms | 1 603 900 |
| labelled, **serial** | 20.8 ms | **21.3 ms** | **199 868** |
| unlabelled, parallel | 132.3 ms | 33.1 ms | 1 603 879 |
| unlabelled, **serial** | 27.4 ms | **26.4 ms** | **199 879** |

The serial path wins at every core count and allocates **8× less**. The cause is
task #2185, landed earlier in this sprint: it gave the *serial* aggregate an unboxed
columnar pre-projection (1.00 allocations per row), while the parallel aggregate scan's
per-worker factory still builds the boxed row-at-a-time `exec.NewProject` (8 per row).
#2185's success inverted the ranking.

So the labelled leaf is **not** admitted on the aggregate path.
`TestParallelLabelAggregate_LabelledLeafStaysSerial` pins that decision with these
numbers, so a future change that gives the workers the unboxed pre-projection has to
re-measure before flipping it.

## 4. Two findings this measurement surfaced

Recorded rather than silently absorbed.

**(a) The already-admitted unlabelled aggregate leaf has become a pessimisation.**
26.4 ms serial against 33.1 ms parallel at 10 cores, and 199 879 allocations against
1 603 879. This is not something #2187 introduced — it is #2185 making the serial path
faster than the parallel one. The fix is to give the parallel workers the same unboxed
columnar pre-projection, not to narrow the leaf. Until then the parallel aggregate scan
is a 1.25× pessimisation on the shape it already claimed.

**(b) `ParallelScanProject` lacks the `budget==1` inline-serial short-circuit its sibling
has.** `ParallelAggregateScan` gained one in #2115 (`parallel_aggregate_scan.go`); the
projection path never did, so at governor budget 1 — which is the **saturated** case, not
merely a single-core host, because the governor divides GOMAXPROCS by the number of
parallel leaves in flight — it still allocates a goroutine, a work channel, a cancellable
context and a `pprof.Do` frame.

A candidate short-circuit was written and measured during this task: at GOMAXPROCS=1 it
produced **no** improvement (89.1 ms before, 89.1 ms after), which locates the single-core
cost in the per-morsel sub-plan rebuild rather than in the concurrency primitives. It was
therefore **backed out** rather than merged on the strength of a plausible-sounding
rationale. The gap is real and worth closing, but the fix must be aimed at the per-morsel
rebuild and validated under genuine concurrency, not at GOMAXPROCS=1.

## 5. Correctness

`cypher/parallel_label_scan_diff_test.go`:

- **Subset-label differential.** On a graph where `:Few` is every third node, the parallel
  arm must return exactly the serial arm's multiset AND exactly `n/3` rows — so a leak
  from the label bitmap to the whole-graph walk shows up as 3× the rows.
- **Exact value set.** The projected values are compared against the set computed
  directly from the fixture, so a correct-count-but-wrong-rows partitioning bug cannot
  hide behind an equal-length comparison.
- **Threshold.** A 10-member label in a 600-node graph must stay serial while the same
  graph's 600-member label engages, proving the gate reads the label's cardinality.
- **Unknown label.** Resolves to an empty bitmap, stays serial, returns nothing.
- **Aggregate decline.** The measured decision of §3, asserted directly, with the
  unlabelled shape still engaging so the test distinguishes the label rule from a
  wholesale disable.
- **`goleak.VerifyNone`** over the projection and aggregate paths.

The shared differential table in `parallel_scan_project_diff_test.go` also gained four
labelled cases (`label-return-prop`, `label-filter-eq`, `label-filter-range`,
`label-multi-col`), and `MATCH (n:Item) RETURN n.v AS v` moved out of its
`DeclinesOutsideScope` list — that entry existed to document the limitation this task
removes.

One precedence fix was required: since the parallel aggregate scan now sees labelled
leaves at all, it was claiming `MATCH (n:L) RETURN count(*)` ahead of the O(1)
`LabelCountScan`, replacing an index read with a full parallel walk. The label-count
pushdown is now tried first, and `TestParallelLabelAggregate_LabelCountStillO1` pins it.

Test power was verified by mutation: leaking the label walker to the whole-graph walker,
and ignoring the label threshold, were each caught.

`go test -race` green, TCK 3897/3897.
