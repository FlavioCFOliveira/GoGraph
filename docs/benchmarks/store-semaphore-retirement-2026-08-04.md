# Retiring the store's single-writer semaphore — rmp #2306 AC1

**Date:** 2026-08-04 · **Machine:** Apple M4, darwin/arm64, 10 logical CPUs ·
**Go:** 1.26.5 · **Harness:** `bench/mvccwrite`, `BenchmarkWriteScaling/wal`

## The claim under test

rmp #2306 AC1 asserts that the `txn.Store` capacity-one semaphore serialised
independent write transactions, and that retiring it would let throughput rise
with writer count.

**The premise is REFUTED. Retiring the semaphore is throughput-neutral.**

## Measurement

Arm A is the semaphore at capacity one, arm B is the semaphore retired and
replaced by the writer-admission gate. Both arms were measured on the same
machine in the same session, interleaved at n=3 first and then repeated at n=6
for `benchstat`, with no competing load and no `-race`.

```
                               │  semaphore   │              retired
                               │    sec/op    │    sec/op     vs base
WriteScaling/wal/writers=1-10    3.383m ±  7%   3.321m ±  5%   ~ (p=0.485 n=6)
WriteScaling/wal/writers=32-10   241.8µ ± 12%   249.9µ ± 12%   ~ (p=0.394 n=6)
geomean                          904.5µ         911.1µ         +0.72%

                               │  commits/s   │  commits/s    vs base
WriteScaling/wal/writers=1-10     295.6 ±  8%    301.1 ±  5%   ~ (p=0.485 n=6)
WriteScaling/wal/writers=32-10   4.413k ± 13%   4.274k ± 11%   ~ (p=0.329 n=6)
geomean                          1.142k         1.134k         -0.69%

                               │   scaling    │   scaling     vs base
WriteScaling/wal/writers=1-10    1.000 ±  0%    1.000 ±  0%    ~ (p=1.000 n=6)
WriteScaling/wal/writers=32-10   15.25 ± 13%    14.41 ± 11%    ~ (p=0.180 n=6)
geomean                          3.905          3.797          -2.78%
```

No difference is significant at any writer count. The full ladder measured at
n=3 agrees: 1 → 2 → 4 → 8 → 16 → 32 writers gives 1.00× → 1.22× → 2.04× →
4.04× → 7.57× → 16.3× with the semaphore retired, and the arm with the
semaphore in place reached 15.25× at 32 writers.

## Why the premise was wrong

The semaphore was released **immediately after the WAL append** and before the
coalesced fsync, so it never covered the part of a durable commit that
dominates. At 4413 commits/s with an append critical section on the order of
tens of microseconds, its utilisation is a few percent — it cannot be the
ceiling. The ceiling is the group-commit fsync, and that already coalesces:
**both** arms scale ~15× at 32 writers.

This is visible in arm A's own numbers rather than inferred. A capacity-one
semaphore held across a commit would cap scaling near 1×; arm A reaches 15.25×,
which is only possible because the section it guarded is short.

The benchmark does exercise the semaphore in arm A — the Cypher WAL commit path
takes it at `cypher/api.go`'s `e.store.BeginCtx(ctx)` before the eager apply —
so the null result is a property of the mechanism, not of an unexercised code
path.

## What the retirement is worth, then

Not speed. It is worth two things, and both are the sprint's actual objective:

1. **MVCC becomes the sole concurrency control.** The semaphore was the last
   mechanism that prevented, rather than detected, a write-write collision. With
   it gone, a collision is arbitrated by first-updater-wins on the version chain
   and reported as a serialization conflict.

2. **The quiesce becomes a genuine quiesce primitive.** The semaphore did two
   unrelated jobs with one lock: serialise writers, and provide the "no commit in
   flight" boundary `RunUnderCommitLock` needs to close or truncate the WAL. The
   replacement keeps only the second — an admission gate that is closed *only*
   while a quiesce runs — merged with the in-flight counter so a writer is
   registered for its whole `Begin`→`Commit` lifetime. That is one window where
   there were two abutting ones.

Equally important: it did not COST anything. Retiring a serialiser that turns
out not to have been a bottleneck could still have regressed the path by adding
accounting; it did not (geomean −0.69% commits/s, not significant).

## What remains owed

`writeScalingFloor` (0.60) and `writeConcurrencyFloor` (0.50) still cannot be
ratcheted to `writeScalingTarget` (3.0). They measure the **store-less** wiring,
which has no store and therefore never had the semaphore; measured this session
at 1.872×–2.067× and 1.584×–2.359×. That ceiling is elsewhere and needs its own
profile. `walWriteScalingFloor` stays at 3.0 and passes (3.779×–3.942× at the
gate's eight writers).

## Behaviour change this surfaced

With nothing serialising writers on either wiring, two concurrent `MERGE`
statements on the same pattern can both find no match and both create. Measured:
**eight duplicates from eight writers**. This is inherent to MVCC — two CREATEs
of two distinct new nodes are not a write-write conflict, so there is nothing to
arbitrate — and it matches Neo4j, which requires a uniqueness constraint for the
same structural reason. With a `UNIQUE` constraint the reservation is atomic and
the duplicates collapse to exactly one, verified stable over twelve runs under
`-race`.

Both outcomes are pinned by `cypher.TestConcurrentMerge_*`, and the operator
documentation under `cypher/exec/` was corrected: `Merge`, `MergePattern`,
`MergeRelationship`, `mergeSearch` and `QueryCounters` all cited a
"single-writer guarantee" that no longer exists.
