# Un-gating the labelled count pushdown — measured

**Task:** rmp #2654 · **Date:** 2026-08-28 · **Machine:** Apple M4, 10 cores, darwin 25.5.0, go1.27.0 darwin/arm64

## What changed

`tryBuildLabelCountScan` gated an **O(1) maintained label-count read** on
`useParallelScan` — that is, on `DefaultParallelScanThreshold` (50 000 live nodes,
strict `>`) **and** on the `DisableParallelScan` feature flag. A *parallelism*
threshold was guarding a *serial constant-time* answer; the two decisions have
nothing to do with each other.

The gate is removed. What remains is a walker guard (`*lpgNodeWalker` with a nil
morsel), byte-for-byte the form the already-un-gated sibling
`tryBuildAllNodesCountScan` uses. That guard is not a new gate: the type assertion
inside `useParallelScan` had been supplying it incidentally, and it is the only
part of the deleted condition that carried correctness — it keeps the recogniser
off a morsel walker (whose share of the rows is not the label's count) and off the
write path's mutator adapters (whose uncommitted writes are not in the committed
label index the fast path reads).

**Exactness was never the planner's job and still is not.** It is enforced one
layer down: `lpgLabelResolver.ResolveLabelCount` → `lpg.Graph.LabelCountExact` is
exact-or-nothing and declines the moment any history is live, whereupon
`exec.LabelCountScan` counts `LabelBitmapAsOf` under the pinned snapshot — the same
source `exec.NodeByLabelScan` reads.

## The plan

```
before (<= 50 000 nodes)              after (every size)
Project                               Project
└─ GlobalAggregateAdapter             └─ LabelCountScan [Item]
   └─ EagerAggregation
      └─ ColumnarProject
         └─ NodeByLabelScan [Item]
```

## Method

One tree, one binary, **one variable** — `EngineOptions.ParallelScanThreshold`.
The *after* arm is reachable pre-fix by setting the threshold to 1, so the whole
before/after is a same-tree, same-host comparison with no code change:

* `before` — `EngineOptions{}` (threshold 50 000) → below it the pushdown declines
* `after` — `EngineOptions{ParallelScanThreshold: 1}` → the gate passes
* `off` — `EngineOptions{DisableParallelScan: true}`

Ten interleaved passes (Go's own `-count` does **not** interleave: it loops
`-count` times per *leaf*, so `-count=8` runs all of A then all of B — interleaving
is driven from outside, one pass per invocation), `benchstat n=10`.

**Every one of the 25 (size × arm) cells had its plan asserted two independent
ways** — exact `Engine.Explain` text matched against one of exactly two constants,
so an unrecognised third plan fails the run, *and* `Engine.Profile` db-hit
accounting (`dbhits=n` at `NodeByLabelScan` against `dbhits=0` at
`LabelCountScan`). Plan text says what will run; db hits say what did. Each timed
loop is bracketed by an `Explain` re-check that fails on plan drift.

**Noise floor**, measured before any delta was quoted, two ways: same-vs-same
within the binary (byte-identical arms at distinct code addresses, so code-layout
noise is included) gave no significant difference anywhere, largest point deviation
0.33%; cross-round cross-binary on identical code gave worst |Δ| = 0.73%. The floor
actually held to is **~2%**, because two identical-plan arms differ reproducibly by
1.45–1.91% at n=1 000 (see *Not established* below). `B/op` and `allocs/op` were
exactly deterministic — all ten samples equal in every cell.

## Result

```
        │     before     │                after                │                    off
        │     sec/op     │   sec/op     vs base                │     sec/op      vs base
001000      30.642µ ± 1%   1.423µ ± 1%  -95.35% (p=0.000)         30.056µ ± 1%       -1.91% (p=0.000)
010000     269.566µ ± 2%   1.420µ ± 1%  -99.47% (p=0.000)        269.748µ ± 1%            ~ (p=0.481)
050000    1316.829µ ± 1%   1.417µ ± 1%  -99.89% (p=0.000)       1322.790µ ± 1%            ~ (p=0.436)
060000       1.420µ ± 1%   1.423µ ± 1%        ~ (p=0.493)       1586.676µ ± 1%  +111637.71% (p=0.000)
100000       1.423µ ± 1%   1.423µ ± 1%        ~ (p=0.926)       2664.108µ ± 2%  +187051.91% (p=0.000)
```

**At n = 50 000** — exactly the strict-`>` boundary, so still the slow arm before the
fix — **929× time, 220× bytes (477 944 → 2 168 B/op), 1 718× allocations
(49 815 → 29 allocs/op)**.

Stated as a user meets it: before the fix the same query cost 1 316 829 ns at
50 000 nodes and 1 420 ns at 60 000 — it became **927× faster on a 20% bigger
graph**. The boundary was bisected: declined at 50 000, fired at 50 001.

**The A/B validated itself.** At 60 000 and 100 000 the arms are statistically
indistinguishable (p=0.493, p=0.926) and byte-identical in allocation, because
above the threshold they *are* the same program. An A/B whose arms fail to converge
where they must be identical is not measuring what it claims.

## It is a complexity change, Θ(n) → Θ(1)

Confirmed in all three dimensions over a 100× range in n. Serial arm:

| dimension | fit | R² |
|---|---|---|
| time | `ns = 586.6 + 26.5631·n` | **0.999963** |
| allocations | `allocs = n − 185.2` | **1.000000** — exactly one allocation per node |
| bytes | `B = 62 675 + 8.2069·n` | 0.998890 |

Log-log exponent k = 0.9728 (R² = 0.999881); ns/node flat at 26.4–27.0 for
n ≥ 10 000. Pushdown arm: `ns = 1420.8 + 0.0000·n` (R² = 0.05, no relationship),
**29 allocations and 2 168 bytes constant, zero variance**.

## `DisableParallelScan` was forfeiting the O(1) count

A flag documented purely as the escape hatch for the *morsel-parallel* path
converted a constant-time labelled count into a full scan:

| n | with pushdown | `DisableParallelScan: true` | penalty |
|---|---|---|---|
| 60 000 | 1 420 ns, 2 117 B, 29 allocs | 1 586 676 ns, 544 938 B, 59 817 allocs | **1 117×** time, +206 166% allocs |
| 100 000 | 1 423 ns, 2 117 B, 29 allocs | 2 664 108 ns, 857 414 B, 99 817 allocs | **1 872×** time, +344 097% allocs |

Below the threshold the flag cost nothing *extra* only because the size gate had
already declined. This is why both couplings were removed, not just the size.

## The gate also read the wrong cardinality

`useParallelScan` thresholds the **whole graph's** `LiveOrder()`, not the label's
own count. Measured before the fix:

* **100** `:Rare` nodes in a 60 000-node graph → `LabelCountScan`, O(1)
* **50 000** `:Item` nodes in a 50 000-node graph → full serial pipeline

The 500× *smaller* count was the one answered in O(1). The codebase already names
this error class: `useParallelScanForRows` exists precisely because gating a
labelled scan on graph order *"would spawn workers for a ten-node label in a
million-node graph"* — yet the labelled count pushdown called `useParallelScan`,
not the labelled sibling.

## Where the cost was

Allocation attribution, `before` arm at n = 50 000, `MemProfileRate=512`, 300
iterations, focused on the timed query path. The probe did not move what it
measured: with the profiler on, `B/op` (477 944) and `allocs/op` (49 815) were
**exactly** the unperturbed values, though ns/op rose 73.6%.

| share | site |
|---|---|
| **83.2%** (113.96 MB, ~99.7% of objects) | `exec.(*NodeByLabelScan).Next` — `cypher/exec/scan_label.go:152` |
| 7.25% | `exec.(*EagerAggregation).consumeChunk` — `cypher/exec/eager_aggregation.go:422` |
| 6.87% | `exec.growTo[int64]` — `cypher/exec/chunk.go:562` |
| 1.80% | roaring `Bitmap.Clone` via `ResolveLabelBitmap` — the bitmap the serial scan must materialise and the pushdown never builds |
| 0.54% | `Engine.buildReadPhysical` — paid by **both** arms; inside the 2 168 B/op the pushdown keeps |

The dominant line is an interface conversion:

```go
// cypher/exec/scan_label.go:152 — 113.96 MB flat, 33.24% of all process bytes
op.buf[0] = expr.IntegerValue(int64(op.iter.Next()))
```

The compiler lowers it to `runtime.convT64`, which returns a pointer into the
runtime's `staticuint64s` table (allocation-free) for values below 256 and
otherwise calls `mallocgc(8, …)`. The model `allocs/op = #{scanned ids ≥ 256} + 71`
was tested against the id distribution **read out of the engine** with `id(n)`:

| n | allocs/op | id range | #{id < 256} measured | model |
|---|---|---|---|---|
| 200 | 144 | [0, 761] | 126 | 127 (off by 1) |
| 256 | 163 | [0, 764] | 164 | **164** |
| 300 | 183 | [0, 994] | 188 | **188** |
| 1 000 | 815 | [0, 1 948] | 256 | **256** |
| 2 000 | 1 815 | [0, 3 324] | 256 | **256** |

**A premise was refuted en route.** The first version of this prediction assumed
node ids are dense from 0 in insertion order, giving `max(0, n−256)`. That holds
exactly for n ≥ 1 000 and **fails** below it (144/163/183 measured against
71/71/115 predicted): the engine does not assign ids densely in insertion order —
the first 256 inserted nodes span id range [0, 764]. Replacing the assumption with
the measured distribution is what turned correlation into attribution.

CPU profile, same arm (profiler perturbed timing by only −1.4%): `runtime.mallocgc`
cum 22.09%, of which `mallocgcTinySC2` 18.45% — the tiny-size-class path, i.e. the
8-byte boxes. With `madvise` (10.92%) and `gcBgMarkWorker` (7.04%), **allocation
plus GC accounts for roughly 40% of all CPU**. Those last two are attributed to the
allocation rate *by consistency* — the process does nothing else during the run and
83% of bytes sit on one line — not by a separate experiment.

## A godoc contract this falsified

`cypher/exec/scan_label.go` asserted, under a heading **"# Zero-alloc contract"**,
that *"Next consumes it one NodeID at a time with zero additional allocations"*.
The reasoning was right about the roaring iterator and the fixed `[1]expr.Value`
buffer, and missed the interface conversion *into* that buffer. Corrected to a
"Per-row allocation" section that separates the three costs. The ≥ 256 threshold
was verified empirically rather than from memory (`testing.AllocsPerRun`:
`n=255 → 0.00`, `n=256 → 1.00`) — which is also **why the false claim survived
review: every fixture used ids below 256.** The boxing itself is untouched here; it
belongs to the per-node heap-shape work.

## Not established

1. **`off` vs `before` at n=1 000: −1.91% (p=0.000), reproducible (−1.45%,
   p=0.001), but NOT attributed.** ≈590 ns absolute. The direction is consistent
   with `before` paying to evaluate a gate that then declines, and it is
   undetectable at n=10 000/50 000 where 590 ns is 0.2%/0.04% of the query. But
   590 ns is more than a few atomic loads should cost and **no experiment
   attributes it**. Hypothesis only; it is the reason the floor is stated as ~2%.
2. **Mutating workloads unmeasured.** The fixtures are read-only after
   construction, so `ResolveLabelCount` always answered exactly. Under live history
   it declines and `LabelCountScan.Init` falls back to a bitmap clone, so the O(1)
   claim may degrade to O(cardinality) there. Correctness of that path *is* now
   covered by test (`TestLabelCount_ReadTxPinsPushedDownCount` asserts the answer
   provably came from the fallback); its **cost** is not measured.
3. **Concurrency unmeasured.** Everything here is single-threaded; contention on
   the label-index cardinality read is unknown.
4. **RAM evidence is allocation rate, not resident heap** — no
   `GODEBUG=gctrace=1`, no steady-state RSS. Storage was out of scope.
5. The serial pipeline's fixed allocation term is F=71 for n ≥ 1 000 and F=70 at
   n=200 (one fewer chunk-growth step); that single allocation was not chased.

## Reproduction

The before/after arms reproduce **at `a0e5a990` (pre-fix)** and cannot be
reproduced on the current tree: the disable seam is unexported, so an external
package can no longer build the slow arm. The harness at
`bench/audit352/labelcount_gate_ab_test.go` was accordingly re-shaped into a
forward O(1) ratchet.

```bash
git checkout a0e5a990    # the pre-fix tree, for the before arm
go test -c -o /tmp/audit352.test ./bench/audit352/
: > /tmp/gate.txt
for i in $(seq 1 10); do
  /tmp/audit352.test -test.run='^$' -test.bench='BenchmarkLabelCountGate' \
     -test.count=1 -test.benchmem >> /tmp/gate.txt
done
benchstat -col /arm -row /n -filter '/arm:(before OR after OR off)' /tmp/gate.txt
benchstat -col /arm -row /n -filter '/arm:(before OR beforedup)'    /tmp/gate.txt   # noise floor
```

Allocation and CPU attribution:

```bash
/tmp/audit352.test -test.run='^$' -test.bench='BenchmarkLabelCountGate/n=050000/arm=before$' \
  -test.count=1 -test.benchmem -test.benchtime=300x -test.memprofilerate=512 \
  -test.memprofile=/tmp/before50k.mprof
go tool pprof -sample_index=alloc_space -focus='runGateQuery' -lines -top /tmp/audit352.test /tmp/before50k.mprof
```
