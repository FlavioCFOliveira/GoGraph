# MVCC C2 — moving reclamation off the commit path (rmp #2308)

Date: 2026-08-04 · Machine: Apple M4, 10 cores, darwin/arm64 · Go 1.26.5

Acceptance criterion 6 of rmp #2308 asks for a commit-latency benchmark before and
after, compared with `benchstat`, and for any regression to be justified in
writing. There is a regression on one shape. It is stated first.

## What was compared

Both arms run **in the same process**, driven by the same loop, because comparing
across two builds on this machine has previously manufactured phantom regressions
from a byte-identical control.

| Arm | Behaviour |
|---|---|
| `async` | The shipped path. A committer charges its versions to the reclamation debt and, once per `reclaimThreshold` (4096), wakes the background vacuum. It never sweeps. |
| `sync` | The placement rmp #2308 removed, reproduced in `graph/lpg/mvcc_vacuum_bench_test.go` as `syncSweepIfDue`. The committer takes the single-sweeper slot and performs a full unbounded seven-store sweep itself when the debt is due. |

Reproduce:

```bash
go test ./graph/lpg/ -run '^$' -bench 'BenchmarkVacuumCommitLatency' -benchmem -count=8 | tee mean.txt
benchstat -col /arm mean.txt

go test ./graph/lpg/ -run '^$' -bench 'BenchmarkVacuumCommitTail' -count=8 | tee tail.txt
benchstat -col /arm tail.txt
```

## Result 1 — one node: async is 3.47 % SLOWER (significant)

`BenchmarkVacuumCommitLatency`, a single writer churning one property on **one**
node.

```
                       │    async    │               sync                │
                       │   sec/op    │   sec/op     vs base              │
VacuumCommitLatency-10   145.6n ± 2%   140.6n ± 1%  -3.47% (p=0.000 n=8)
```

Allocations are identical in both arms (80 B/op, 3 allocs/op, `p=1.000`).

**This is the regression, and it is real.** A one-node graph has exactly one
version chain, so the sweep the async arm avoids costs almost nothing, while the
bookkeeping it adds — one atomic add and one comparison per commit, plus one
non-blocking channel send per 4096 versions — is not free. On this shape the move
can only cost, which is precisely why it is the shape to measure before claiming
the move is free.

## Result 2 — 16 384 nodes: async is 2.79 % FASTER (significant)

`BenchmarkVacuumCommitTail`, writers churning 16 384 distinct nodes round-robin,
so the sweep has that many chains to walk — the shape a real write workload has.

```
                              │    async    │               sync                │
                              │   sec/op    │   sec/op     vs base              │
VacuumCommitTail/writers=1-10   9.686m ± 1%   9.957m ± 1%  +2.79% (p=0.010 n=8)
VacuumCommitTail/writers=8-10   10.34m ± 1%   10.43m ± 1%       ~ (p=0.505 n=8)
geomean                         10.01m        10.19m       +1.81%
```

The sign has flipped, and the crossover is exactly the sweep's own cost: once
there is real garbage to walk, not paying for it on the commit path wins back more
than the bookkeeping costs.

## Result 3 — the latency distribution is INDISTINGUISHABLE

This is the claim the measurement refused to support, and it is recorded because
the hypothesis going in was the opposite one.

```
                              │    async     │                sync                │
                              │   p50-sec    │   p50-sec     vs base              │
VacuumCommitTail/writers=1-10    229.5n ± 9%   208.0n ± 20%       ~ (p=0.445 n=8)
VacuumCommitTail/writers=8-10   2.355µ  ± 6%   2.396µ ± 10%       ~ (p=0.326 n=8)

                              │    async     │                sync                │
                              │   p99-sec    │   p99-sec     vs base              │
VacuumCommitTail/writers=1-10   771.0n ± 19%   875.0n ± 19%       ~ (p=0.241 n=8)
VacuumCommitTail/writers=8-10   3.604µ ±  7%   3.688µ ±  7%       ~ (p=0.817 n=8)

                              │     async     │                sync                 │
                              │   p999-sec    │   p999-sec    vs base               │
VacuumCommitTail/writers=1-10   3.292µ ±  51%   2.855µ ± 21%        ~ (p=0.505 n=8)
VacuumCommitTail/writers=8-10   10.35µ ± 100%   14.54µ ± 48%        ~ (p=0.328 n=8)

                              │     async     │                sync                │
                              │    max-sec    │    max-sec     vs base              │
VacuumCommitTail/writers=1-10   39.52µ ± 256%   82.42µ ±  39%        ~ (p=0.105 n=8)
VacuumCommitTail/writers=8-10   64.67µ ± 239%   60.13µ ± 341%        ~ (p=0.798 n=8)
geomean                         50.55µ          70.39µ         +39.25%
```

The expectation was a clear tail win: "one commit in four thousand pays for
everybody's garbage" should show up as a worse `max` for the sync arm. The geomean
does lean that way — +39.25 % for sync — but the per-sample variance is 39 % to
341 % and no quantile reaches significance. **So the tail claim is not made.**

An earlier version of this benchmark churned one node and found both arms
indistinguishable at every quantile. That was a null result about the *workload*,
not about the placement: with one chain to sweep there was nothing for a committer
to pay for. Re-running on 16 384 nodes is what produced Result 2.

## Why the change ships despite Result 1

Not for speed. The justification is soundness, and the 3.47 % on the degenerate
shape is the price:

1. **The old placement's contract no longer holds.** `ReclaimNow` demanded "the
   visibility barrier in write mode, or otherwise exclude concurrent writers", and
   rmp #2307 removed the barrier from the write path. Reclamation had no exclusion
   left to stand on. It now stands on the per-shard lock each reclaimer takes,
   verified body by body in `Graph.sweepUnit`.
2. **One reclaimer genuinely was not writer-safe**, and the audit that criterion 4
   asked for found it: `applyDeferredIndexRemovals` released `idxDeferred.mu`
   before removing entries from the label bitmap, so a concurrent re-add of the
   same label lost its index entry permanently — a node carrying a label and
   absent from every later label scan. Reproduced within 2 rounds under `-race`
   (`TestDeferredIndexRemoval_ConcurrentReaddIsNotLost`) and fixed.
3. **A committer's cost no longer depends on other transactions' garbage**, which
   is what the bounded-resources and fair-scheduling mandates ask for.
4. On any realistic shape the move is neutral to positive (Result 2), and nothing
   regressed in allocations.

## The memory trade the change makes

Reclamation became asynchronous, so the churn bound is no longer instantaneous.
The module now states two numbers, both exported on `MVCCStats`:

| Quantity | Value | Meaning |
|---|---|---|
| `Bound` | `reclaimThreshold` = 4096 | The settled bound. Churn returns to it once writing stops. |
| `Ceiling` | `reclaimDebtCeiling` = 16384 | The instantaneous bound. At it, a committer stops signalling the vacuum and waits for one pass (`Graph.awaitVacuumProgress`, ≤ ~6.4 ms). |

Measured peaks before the ceiling existed, over 24 576 modifications: 9 232
retained transactionally and 14 589 through the direct API, against a stated bound
of 8 192. With the ceiling: 8 480.
