# Where the ~2.2× in-memory write ceiling actually is — rmp #2338 (SPIKE)

**Date:** 2026-08-07
**Branch:** `sprint-335`
**Outcome:** the SPIKE's own premise is **REFUTED**. rmp #2339 as scoped would deliver
at most ~3.6% and is a **NO-GO** in that form.

## What the ticket asserted

> The mechanism is a lock NESTING — `graph/lpg/lpg.go` takes the per-shard label lock
> `sh.mu` (1 of 64 shards) and, inside it, reaches the single global `label.Index`
> whose one `sync.RWMutex` is at `graph/index/label/index.go:59`. A per-shard lock
> therefore encloses a global one and all 64 shards funnel through it.

The evidence offered was a mutex profile: *96.9% of mutex delay flows through cypher
`RunInTx` and tops out in `graph/lpg.setNodeLabelInfo`*. rmp #2339 was written to
dissolve that nesting and lift the ceiling above 2.2×.

**The attribution does not survive measurement.** A mutex profile says where *waiting
is recorded*, not what *limits throughput*. Removing the named mechanism moves the
ceiling by nothing.

## Method

Four measurement-only arms, each built as a separate test binary from the same tree
and run **alternately** against a HEAD binary so host drift cancels. Every arm below
is **UNSOUND** and none was merged; they exist only to bound a share.

Apple M4 (10 cores: 4P+6E), darwin/arm64, quiet machine,
`BenchmarkWriteScaling/mem -benchtime=200000x`, benchstat.

The reported quantity is the **scaling factor** — throughput at N writers over
throughput at 1 writer of the identical workload — because that, not absolute
commits/s, is what a "ceiling" means. Throughput is reported alongside, and the
difference between the two columns is the whole finding.

## Results

| Arm (unsound, attribution only) | Scaling ceiling | Throughput | n |
|---|---:|---:|---:|
| **A1** — un-nest: release `sh.mu` before touching `label.Index` | **+0.61% ~** (p ≥ 0.31) | +0.73% ~ | 5 |
| **A2** — A1 **plus** delete the global `label.Index` maintenance outright | **+3.57%** | +3.6% | 5 vs 10 |
| **A3** — delete the **whole** label subsystem (shard lock, bag, deltas, conflict check, index) | **−1.36% ~** | **+10.95%** (p<0.001) | 5 vs 15 |
| **A4** — `GOGC=off` on unmodified HEAD | **+9.19%** (p=0.001) | **+21.64%** | 7 |

Per-writer detail for the two arms that matter:

**A3 — the whole label subsystem removed.** Throughput rises by a near-constant
~8–14% at *every* writer count, **including one writer** (+12.42%), while the scaling
factor does not move:

| writers | scaling HEAD | scaling A3 | commits/s HEAD | commits/s A3 |
|---:|---:|---:|---:|---:|
| 1 | 1.000 | 1.000 | 346.5k | 389.5k (+12.42%) |
| 4 | 2.134 | 2.060 | 743.3k | 804.9k (+8.28%) |
| 16 | 2.215 | 2.176 | 765.3k | 844.9k (+10.39%) |
| 32 | 2.210 | 2.155 | 760.7k | 842.1k (+10.70%) |

A uniform gain at every concurrency level, present at **one writer where there is
nothing to contend with**, is the signature of **per-operation COST, not contention**.
The label subsystem is ~11% of what a `CREATE (n:Account {id: $id})` costs. It
contributes **nothing** to the ceiling.

**A4 — `GOGC=off`.** This one moves the *scaling factor*, which is what a ceiling
contributor looks like:

| writers | scaling default | scaling GOGC=off | Δ | p |
|---:|---:|---:|---:|---:|
| 2 | 1.586 | 1.702 | +7.31% | 0.001 |
| 4 | 2.080 | 2.542 | **+22.21%** | 0.001 |
| 8 | 2.079 | 2.188 | +5.24% | 0.001 |
| 16 | 2.182 | 2.396 | +9.81% | 0.001 |
| 32 | 2.238 | 2.503 | +11.84% | 0.001 |

## What this establishes, and what it does not

**Established.**

1. The bag-plus-index **nesting owns none of the ceiling**. Un-nesting is
   statistically indistinguishable from HEAD at every writer count.
2. The **global `label.Index` lock owns at most ~3.6%** — measured by deleting it
   entirely, which is a strictly stronger change than any sharding scheme could be.
   Sharding it can therefore never beat +3.6%.
3. The **whole label subsystem owns 0% of the ceiling** and ~11% of the per-commit
   constant. Any work aimed at it is a *cost* optimisation, not a *scaling* one, and
   should be scoped and measured as such.
4. The **garbage collector is the largest single contributor found so far**, worth
   ~9% of the scaling factor and ~22% of throughput. It is real but partial: the
   ceiling moves 2.2× → 2.4×, not 2.2× → 10×.

**Not established, and stated as unattributed rather than guessed.** After removing
the entire label subsystem and disabling the GC, the ceiling is still ~2.4× on a
10-core machine. **Roughly 4× of the gap has no owner yet.** No claim is made here
about where it is. The candidates not yet ablated include the mapper's intern path
(every `CREATE` interns a new node through a shared counter), `mvcc.Clock.finishCommitTS`
(which takes a process-global `pubMu` on the publish of *every* commit), the count
store, the plan cache, and the Go scheduler itself. Each needs its own ablation arm;
none is asserted.

## Why the original profile misled

The mutex profile *is* accurate about where goroutines wait: 82% of the delay inside
`setNodeLabelInfo` is `sh.mu`, and 18% is `label.Index.Add`. What it cannot say is
whether removing that wait raises throughput — and it does not, because the work
simply waits somewhere else. A1 demonstrates this directly: un-nesting shortens the
shard lock's critical section and converts shard-lock wait into index-lock wait, with
the total unchanged.

This is the same trap recorded for rmp #2332: *a profile cannot attribute a delta*.
The instrument that can is a cumulative-prefix ablation ladder, which is what this
spike ran.

## Reproducing

Each arm is a one-file edit to `graph/lpg/lpg.go` plus a separate
`go test -c` binary; the arms are then run alternately, e.g.:

```
go test -c -o ws_HEAD.test ./bench/mvccwrite/
# apply the arm's edit, rebuild as ws_ARM.test, restore the tree
for i in 1 2 3 4 5; do
  ./ws_HEAD.test -test.run='^$' -test.bench='BenchmarkWriteScaling/mem' \
      -test.benchtime=200000x -test.count=1 >> head.txt
  ./ws_ARM.test  -test.run='^$' -test.bench='BenchmarkWriteScaling/mem' \
      -test.benchtime=200000x -test.count=1 >> arm.txt
done
benchstat head.txt arm.txt
```

`GOGC=off` needs no edit at all — it is the cheapest arm and the most informative one
run here.
