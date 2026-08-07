# Where the ~2.2× in-memory write ceiling actually is — rmp #2338 (SPIKE)

**Date:** 2026-08-07
**Branch:** `sprint-335`
**Outcome:** the SPIKE's own premise is **REFUTED**. rmp #2339 as scoped would deliver
at most ~3.6% and is a **NO-GO** in that form. The ceiling is set by the commit's
**allocation rate**, not by any lock: a share-nothing, lock-free workload matched to a
commit's cost and allocation profile ceilings at ~2.6× on this host, and GoGraph
already reaches 86% of that.

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

## The ~4× that appeared to be missing was never available

The four arms above left the ceiling at ~2.4× and roughly 4× unattributed. That
remainder has now been measured, and it is **not GoGraph's to recover**.

### The control that was being compared against was the wrong one

`gate_test.go`'s `control/parallel` measures **6.19–6.28×** at eight writers at this
head — a CPU-bound spin that shares nothing *and allocates nothing*. Comparing an
engine commit against it charges the concurrency control for a cost the Go runtime
imposes on any program of this allocation profile, because **one
`CREATE (n:Account {id: $id})` costs 56 allocations and 4242 B**, measured with
`-benchmem` and flat in the writer count (56/4242 at one writer, 55/4103 at eight).

### The allocation-matched control

`bench/mvccwrite/alloc_control_test.go` (`BenchmarkAllocScalingControl`) is N
goroutines each performing a unit that allocates the same count and volume as one
commit and is padded with a non-allocating spin to the same per-unit cost. It shares
**nothing**: no map, no counter, no lock, no critical section. `calibrateSpin`
resolves the pad on the host actually running it, so the match is measured rather
than inherited — 2873 ns/unit against the commit's 2892 ns, 55 allocs/4400 B against
56/4242.

Whatever it scores is the ceiling a **perfectly parallel** Go program of this
allocation profile reaches on this machine.

| writers | alloc-matched ceiling | GoGraph engine | engine ÷ ceiling |
|---:|---:|---:|---:|
| 1 | 1.000 | 1.000 | 100.0% |
| 2 | 1.837 | 1.575 | 85.7% |
| 4 | 2.615 | 2.133 | 81.6% |
| 8 | 2.611 | 2.107 | 80.7% |
| 16 | 2.560 | 2.231 | 87.1% |
| 32 | 2.591 | 2.241 | 86.5% |

*(n=5 each, means; the two benchmarks share `runArm`, so the harness is identical.)*

**A lock-free, share-nothing workload allocating at a commit's rate ceilings at
~2.6× on this host.** GoGraph reaches **2.24×, which is 86% of that.**

### What this means

The "missing 4×" was an artefact of comparing against 10 equal cores, or against a
non-allocating control. It does not exist. The real distance between GoGraph's write
path and the achievable ceiling is **~13–19%**, not 4×.

**The binding constraint on in-memory write scaling is the ALLOCATION RATE of a
commit — 56 objects and 4.2 KB per `CREATE` — and not any lock in the concurrency
control.** That is consistent with, and explains, arm A4: the GC is the visible half
of the same cost, and turning it off recovers part of the same ceiling.

The lever that raises write scaling is therefore reducing allocations per commit,
which raises the ceiling itself. Restructuring the label index, sharding it, or
dissolving the joint transition cannot: they were measured at +0.6%, ≤+3.6% and
−1.4% respectively, and the remaining headroom above them is ~13%, not 350%.

### Still unattributed, and small

The 13–19% between GoGraph and the allocation-matched ceiling has no owner yet. The
candidates are the ones the mutex profile names and this spike did **not** ablate:
the mapper's intern path, `mvcc.Clock.finishCommitTS`'s process-global `pubMu` (taken
on every commit's publish, 4.9% of `sync.Mutex` delay), the plan cache (8.3%), and the
count store. None is asserted here. They are worth ablating only once the allocation
rate is addressed, because they are bounded above by 19% of the current ceiling.

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

## The allocation ledger — where the 56 objects are (rmp #2339's starting point)

Measured, not estimated: `-memprofile` at `-memprofilerate=1` over 200 000 commits of
`BenchmarkWriteScaling/mem/writers=1`, 11 336 561 objects total = **56.7 per commit**.
Flat objects per commit, per call site:

| allocs/commit | call site |
|---:|---|
| 4.01 | `cypher/exec.(*CreateNode).Next` |
| 4.00 | `cypher.buildPlanWithMutatorFull` |
| 3.00 | `cypher.buildPropsEvalFn` |
| 3.00 | `cypher.(*undoLog).record` |
| 3.00 | `cypher.(*Engine).runInTxSession` |
| *2.99* | *`bench/mvccwrite.commit` — the benchmark's own params map, NOT GoGraph* |
| 2.85 | `graph/lpg.(*nodePropShard).pushPropDelta` |
| 2.85 | `graph/lpg.(*nodeLabelShard).pushLabelDelta` |
| 2.00 | `cypher.execUnderBarrier.func3` |
| 2.00 | `cypher/exec.splitMapItems` |
| 2.00 | `cypher/exec.NewCreateNode` |
| 2.00 | `cypher/exec.(*IndexBuffer).Enqueue` |
| 1.85 | `graph/lpg.(*Graph).noteNodeLife` |
| 1.00 each | `ReadAt`, `propBag.set`, `reserveConstraintValue`, `parsePropLiteralWithParamsCtx`, `mergeProps`, `copyLabels`, `exec.Run`, `NewSingleRowOperator`, `newResultWithLimit` |

Two observations that should shape the work, and one warning:

- **~3 of the 56 are the benchmark's own**, not the engine's: `bench/mvccwrite.commit`
  builds a fresh `map[string]expr.Value` per call. Any headline "allocs per commit"
  figure must exclude it or it will claim a win that belongs to the harness.
- **The write path builds its physical plan per statement.**
  `buildPlanWithMutatorFull` is 4.00 flat and **15.0 cumulative** allocs/commit —
  26.5% of everything. The read path has a plan cache; the write path does not,
  because the operator tree it builds has the statement's mutator and its
  transaction-bound views wired into it. Caching it is an **architectural** change to
  the write path, not an allocation tidy-up, and it must be scoped and decided as one
  rather than attempted as part of a sweep.
- The two `push*Delta` sites (5.7 combined) are MVCC's own version records. They are
  the substrate's cost of isolation and are the least likely to be removable without
  weakening a guarantee.

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
