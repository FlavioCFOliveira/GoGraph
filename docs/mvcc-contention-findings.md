# Sprint 336 contention findings: what was removed, and what turned out not to be there

Three mechanisms the audit named as contention were examined. **All three came back neutral
or negative.** This records each with its evidence, because a negative result that is not
written down gets re-proposed.

| mechanism | task | outcome |
|---|---|---|
| duplicated `UNIQUE` registry lock | #2358 | **removed**; no latency change |
| doubled mapper shard acquisition | #2360 | **removed**; geomean −0.28% |
| graph-level `writeCtx` free slot | #2361 | **removal reverted** — it upholds an allocation invariant |
| paired atomic counters (`nodeLifeActive`/`lifeSeq`) | #2363 A | **refused** on range safety |
| count-store padding + fast reject | #2363 B | **done** |

Against `docs/benchmarks/mvcc-contention-arms-2026-08-08.md`, where the ≈2× ceiling is
reached at **four** writers and unmoved at 16 or 32.

**The conclusion those results support:** the write deficit is *not* an accumulation of
per-write traffic on already-sharded or already-local state. Whatever binds it is reached
almost immediately and is unaffected by removing per-write work. Of the named candidates only
**#2362 — `pubMu`, one mutex taken once per commit by every writer** — is a genuinely
serialising structure, and it is the only one that fits the measured signature. See
`docs/mvcc-publish-fast-path.md`.

## #2361 — the `writeCtx` free slot: neither deletion nor the embed removes it

**Finding 1: deleting it is not available.** Allocating a `writeCtx` per bracket instead of
caching one measured *free* — interleaved, n=10, geomean −0.34%, `allocs/op` unchanged on
every arm. Then `make ci` failed:

```
TestBarrierGuard_ApplyAtomicallyAllocatesNothing
  Graph.ApplyAtomically allocated 1 objects per call, want 0
```

The arms drive the Cypher engine path at 41–43 allocations per op, where one allocation per
bracket is statistically invisible; the pin measures the bare-API path, where it is the
*entire* quantity. Reverted.

So the task's premise — that the slot "delivers the contention of a shared word AND the
allocation it was meant to avoid" — is **half wrong**. It does avoid the allocation, on the
path the pin covers.

**Finding 2: the embed cannot remove it either.** `Graph.ApplyAtomically` and
`Graph.ApplyAtomicallyTx` are public bare-API brackets; both call `openWriteBracket` and
**neither has a caller transaction object to embed into** — and they are exactly the paths
the allocation pin covers. So embedding on the caller's transaction cannot delete the
graph-level slot; something must still supply zero-allocation state for bare-API callers.

**The achievable design, corrected.** Embed the `writeCtx` on the *Cypher* transaction, which
does have somewhere to hold it (`tx.wtx`), and **keep** the graph-level slot for bare-API
callers. The shared word survives but is no longer touched by the production writer, which is
where the contention was claimed. That is narrower than the task described, and it is what is
actually on offer.

**Whether it is worth doing is now doubtful** and should be decided before the work, given
that the two mechanisms already removed both measured neutral and the ceiling is reached at
four writers. If it is done: preserve the `WriteTx` lifetime contract, pin that using a
`WriteTx` after its bracket is still refused, check the autocommit path as carefully as the
explicit one, and keep `TestBarrierGuard_ApplyAtomicallyAllocatesNothing` passing **unedited**
— it is the invariant that killed the shortcut.

## #2363 Item A — folding paired counters: refused on range safety

The task makes range safety its own gating condition: *"a packed field silently wrapping into
its neighbour is a corruption, not a slowdown. Pick the bit widths from the real maxima."*
The real maxima cannot be established for this pair:

- `nodeLifeActive` is a **decrementable** count of live existence records, bounded only by
  churn and by how long a reader retains versions. No hard ceiling exists in the code or docs.
- `lifeSeq` is **monotonic** and orders two existence events a commit timestamp cannot
  separate (see `lifeStamp`), so a wrap mis-orders node visibility.

Any bit split therefore risks the count overflowing into the sequence and silently corrupting
the ordering of node existence — trading a correctness property for a win the task's own
requirements call small and explicitly *not* expected to move the deficit. Under
`correct → secure → fast` that trade is refused. Reopen only with a hard, documented ceiling
for `nodeLifeActive` plus the boundary test the requirements demand.

## #2363 Item B — count store: done

MySQL's `Trx_shard` is `ut::Cacheline_padded<ut::Guarded<...>>` holding a
`std::atomic<trx_id_t> m_min_id` (`storage/innobase/include/trx0sys.h`), so a lookup can be
rejected **before the latch is acquired**. Three parts: padded shard, own latch, atomic
fast-reject. This store had the middle one only; it now has all three.

- **Padding**: `shard` is rounded to a 128-byte cache line (128 rather than 64 because Apple
  silicon's line is 128 and x86 prefetches line pairs). The filler is a *computed* expression,
  so adding a field cannot silently defeat the padding — the build fails instead. Cost stated:
  8 KiB of shard array at `numShards=64` against ~4 KiB unpadded, once per `Store`.
- **Fast reject**: `shard.cells` counts cells across all four maps, maintained in exactly one
  place — `Store.add`, the sole insert/delete routine — under the write lock, so it cannot
  drift from the maps it describes. `CountE`, `CountD` and `CountT` return 0 without taking
  `mu` when it reads zero.

Safe by direction, not by luck: `cells` is incremented **before** the write lock is released,
so a non-zero value always routes the reader down the locked path. A zero read means no cell
existed at some instant within the call, and a concurrent insert racing the read is the same
race the `RLock` version already has — either answer is a legal snapshot read. What it can
never do is *miss* a cell that was already there.
