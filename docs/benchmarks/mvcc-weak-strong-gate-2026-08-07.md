# A weak/strong gate to replace the shared-counter RWMutex — 2026-08-07

rmp #2337. Head `bd4efac4`, branch `sprint-335`. Host: Apple M4, 10 cores,
`darwin/arm64`, idle.

```
go test -run '^$' -bench 'BenchmarkGate_WeakParallel|BenchmarkRWMutexShared_Parallel' \
    -cpu=1,2,4,8,10 -count=5 ./graph/mvcc/
```

Harness: `graph/mvcc/gate_test.go`. Both arms measured in the same invocation.

## Why this exists

Two barriers are taken SHARED by every ordinary write and EXCLUSIVELY by DDL: the
Cypher engine's `schemaMu` (`cypher/api.go`, taken shared at the top of every
statement) and lpg's `visMu` (`graph/lpg/lpg.go:616`, taken shared inside every
apply bracket). Neither excludes writers from one another. Yet Go's `sync.RWMutex`
implements the shared acquisition as an atomic add on ONE `readerCount` word, so a
write pays a coherence miss on a shared line **twice**, purely to announce a
non-conflict.

MVCC cannot subsume these: they guard the CATALOG, which is not versioned. Both
reference engines keep the same weak/strong split — Memgraph's `main_lock_` is a
`std::shared_lock` for an ordinary write and unique for index/constraint
transitions; PostgreSQL encodes it in `LockConflicts`, where DML's
`RowExclusiveLock` does not self-conflict and `CREATE INDEX`'s `ShareLock` does. So
what is replaceable is the **implementation**, not the mechanism.

## The measurement

Lower is better; these are per-operation times under `RunParallel`.

| GOMAXPROCS | 1 | 2 | 4 | 8 | 10 |
|---|---|---|---|---|---|
| `Gate` weak acquire | 3.77 ns | 1.82 ns | 0.925 ns | 0.519 ns | **0.434 ns** |
| `sync.RWMutex` shared | 3.75 ns | 8.78 ns | 17.0 ns | 87.0 ns | **89.5 ns** |
| Gate advantage | 1.0× | 4.8× | 18.4× | 168× | **206×** |

**The gate scales; the RWMutex degrades.** From 1 to 10 cores the gate gets 8.7×
faster per operation (near-linear on 10 cores), while the RWMutex gets **23.9×
slower** — consistent with the 17.6× degradation rmp #2203 measured for the same
shape, and confirming that the shared reader counter, not the surrounding code, is
what stops it scaling.

At one core the two are at parity (3.77 vs 3.75 ns), so the gate costs nothing on a
single-core deployment.

## THE FIRST DESIGN FAILED THIS BENCHMARK, AND THAT IS THE POINT

The first version chose its stripe with `next.Add(1)` on a shared atomic, copying
[`Horizon.claim`]. Measured on the same host:

| GOMAXPROCS | 1 | 2 | 4 | 10 |
|---|---|---|---|---|
| `Gate` (rotating counter) | 2.98 ns | 30.9 ns | 30.5 ns | 50.4 ns |
| `sync.RWMutex` shared | 3.71 ns | 8.8 ns | 16.9 ns | 90.1 ns |

It was **3.5× WORSE than the lock it replaces at 2 cores**, because every
acquisition wrote one shared word and that word became the bottleneck. PostgreSQL's
`src/backend/storage/lmgr/README` (commit `0ec3f048bfc15c8eb9933e8228b847593389da1b`,
read 2026-08-07) names this failure exactly:

> it must be possible to verify the absence of possibly conflicting locks without
> fighting over a shared LWLock or spinlock. Otherwise, this effort would simply
> move the contention bottleneck from one place to another.

The fix was to delete the shared counter outright: the stripe index now comes from
a caller-supplied `hint`, and the intended source is the transaction id the caller
already holds (`Clock.NextTxID` mints them sequentially, so concurrent transactions
land on distinct stripes). Correctness does not depend on the hint at all — a
collision costs a shared cache line, never admission or safety.

`Horizon.claim` keeps its rotating counter and is NOT implicated: it runs once per
read transaction, not once per statement, and `horizon.go` records it measuring flat
at 2.135 ns.

## Correctness

`graph/mvcc/gate_test.go`, green under `-race -count=3`.

The exclusion oracle is the **race detector**: `guarded` is a plain unsynchronised
`int` that weak holders read and strong holders write, so any overlap is a reported
data race rather than an assertion that happens to sample the right instant.

**The instrument was validated against a defective build.** Disabling the drain
loop in `StrongLock` makes `TestGate_StrongExcludesWeak` fail with BOTH oracles
firing — `WARNING: DATA RACE` and `strong holder ran beside 1 weak holders` — so the
test is known to be capable of catching the defect it guards. The defect was then
reverted and the suite re-run green.

One oracle was wrong first time round and is recorded here because it is a trap:
`Gate.WeakHolders` counts raw stripe claims, which include an acquirer that has
claimed a stripe and is about to back out on finding a strong holder. Such a
goroutine never enters its critical section, so using that counter as the exclusion
oracle reports violations that did not happen. The test counts goroutines actually
inside the critical section instead, and `WeakHolders` is now documented as a gauge
that must not be used as an exclusion oracle.

## Status — NOT yet adopted

`Gate` is a self-contained primitive with no callers. Swapping it in for `visMu` and
`schemaMu` needs one capability it does not yet have: a **context-bounded weak
acquisition**. `ApplyVersionedCtx` and `ApplyInVersionedTx` currently acquire through
`ctxlock.Acquire(ctx, visMu.TryRLock, visMu.RLock, visMu.RUnlock)` so a writer queued
behind a DDL honours its caller's deadline (rmp #2174, #2306) — a bound that exists
because ignoring it once measured **ten minutes against a 200 ms context**. That
bound must not be lost, so `Gate` needs `TryWeakLock`/`WeakLockCtx` before it can
replace either barrier.

The re-entrancy guard (`graph/lpg/reentrancy_enabled.go`) also keys on visMu's
semantics and will need to be re-established against the gate.

## ADOPTED for `schemaMu` — and the end-to-end result is modest

`cypher.Engine.schemaMu` (a `sync.RWMutex`) is now `cypher.Engine.schemaGate` (an
`mvcc.Gate`). Six call sites: five DDL acquisitions became `StrongLock`/`StrongUnlock`
(`runCreateBTreeIndex`, `runDropIndex`, `runCreateConstraint`, `runDropConstraint`,
and the one in `index_binding.go`), and the single weak acquisition taken by every
ordinary write statement in `runInTxSession` became `WeakLockAuto`/`WeakUnlock`.

`schemaMu` needed no context-bounded acquisition — `internal/ctxlock` is used only
with `visMu` — which is why it could be adopted first.

### End-to-end effect: NOT ESTABLISHED — the across-time comparison below is INVALID

`BenchmarkWriteScaling/mem`, same host and harness, n=5, before vs after:

| writers | 1 | 4 | 8 | 16 | 32 |
|---|---|---|---|---|---|
| before (commits/s) | 333.4k | 678.2k | 700.9k | 739.6k | 742.4k |
| after (commits/s) | 336.8k | 736.2k | 728.4k | 757.2k | 765.9k |
| change | +1.0% | **+8.6%** | +3.9% | +2.4% | **+3.2%** |
| scaling after | 1.00× | 2.19× | 2.16× | 2.25× | **2.27×** |

**The ceiling did not move: 2.23× before, 2.27× after.** A 206× improvement on the
primitive bought about 3% end-to-end, and that is the honest and expected result —
it says the barrier was NOT the binding constraint. The binding constraint remains
the one the mutex profile attributed in
`mvcc-storeless-write-ceiling-2026-08-05.md`: `setNodeLabelInfo` nesting the single
global `label.Index` lock inside the per-shard label lock, which is rmp #2338/#2339.

This is worth stating plainly because it is the sort of result that invites
overclaiming. The gate is the right structure and it removes one of the two shared
cache lines a write pays; it does not on its own make writes scale, and no
measurement here suggests it would.

### Still on `sync.RWMutex`: `visMu`

`graph/lpg.Graph.visMu` is unchanged. It is the harder of the two: it is acquired
through `ctxlock.Acquire(ctx, visMu.TryRLock, visMu.RLock, visMu.RUnlock)` so a
writer queued behind a DDL honours its caller's deadline, and its exclusive mode is
policed by the re-entrancy guard in `graph/lpg/reentrancy_enabled.go`. `Gate` now has
`TryWeakLock` and `WeakLockCtx` covering the first requirement; the guard is the
remaining piece.

## RETRACTION: the end-to-end numbers above are not trustworthy

The `schemaGate` before/after table earlier in this file compares two
`BenchmarkWriteScaling/mem` runs taken **at different times**, and that comparison is
invalid. The host drifts enough between runs to manufacture a result in either
direction from identical code, which was demonstrated accidentally and then
deliberately:

- with both barriers on the gate, 4 writers measured 536.3k commits/s;
- reverting ONLY the `visMu` swap and re-measuring **back-to-back** gave 547.6k;
- but the SAME `schemaGate`-only configuration had measured **736.2k** an hour earlier.

So the apparent "-27% regression" from the `visMu` swap and the apparent "+8.6% gain"
from the `schemaGate` swap are the SAME artefact with opposite signs. Neither is real.

**What is established:** the primitive's own scaling (the first table — both arms in
one invocation, which is why that one is sound), and that both swaps are
performance-neutral end-to-end within noise when measured back-to-back.

**What is NOT established:** any end-to-end throughput claim for either swap. A real
figure needs interleaved A/B arms in a single invocation, the way the primitive
benchmark is built. That is the only comparison shape this file should ever have used.

The write-scaling ceiling is unchanged at roughly 2.0-2.3x either way, and it is set
by the `label.Index` nesting of rmp #2338/#2339, not by these barriers.

## Both barriers are now on the gate

`cypher.Engine.schemaMu` and `graph/lpg.Graph.visMu` are both gone; the fields are
`schemaGate` and `visGate`, and no `sync.RWMutex` remains behind either DDL-exclusion
barrier. `visGate` needed two additions to `Gate` — `WeakLockCtx`/`WeakLockCtxAuto`
and `StrongLockCtx` — because `internal/ctxlock` bounded both its shared and its
exclusive acquisition so a caller behind a DDL honours its deadline (rmp #2174,
#2306). `internal/ctxlock` now has no code callers.

The re-entrancy guard needed NO change: it tracks goroutine identity and never
depended on the primitive's type. One test did —
`waitUntilQueuedOnWriteLock` polled the runtime stack for a `sync.(*RWMutex).Lock`
frame that no longer exists, so it silently never matched and reported the guard as
broken when the guard was fine. It now matches the `Gate.StrongLock` frame.
