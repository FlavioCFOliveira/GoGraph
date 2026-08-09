# The commit-publication fast path: the `l.oldest` question, settled (rmp #2362)

`Clock.finishCommitTS` takes `pubMu` once per commit, for every writer. rmp #2362 proposes
a lock-free fast path in front of it. That task's technical requirements demand one question
be settled **in writing before any code**, because a wrong answer "stalls the visibility
frontier permanently". This is that answer, and it adds a condition the task did not state.

## Finding 1 — `l.oldest` *is* derivable from `visible`

`commitLog.frontier()` returns `l.oldest - 1`. `Clock.finishCommitTS` stores `visible` only
when `frontier > visible`, and `l.oldest` only ever advances. So

```
visible == l.oldest - 1
```

holds as an invariant. A slow path can therefore **derive** `oldest = visible + 1` instead
of trusting its own field, and the fast path does not need to record the bit for `ts`:
CASing `visible` from `ts-1` to `ts` is exactly equivalent to advancing `oldest` from `ts`
to `ts+1` — *provided nothing above `ts` is already finished.*

## Finding 2 — the proposed one-condition guard STALLS THE FRONTIER

This is the part the task missed, and it is not a rare interleaving.

`commitLog.finish(ts)` with `ts == oldest` calls `advance()`, which walks forward over
**every contiguous set bit** and can jump the frontier by many. The proposed fast path
advances by exactly **one**. So:

```
ts+1 … ts+5 finish OUT OF ORDER   (bits set under the lock, frontier unmoved)
ts finishes via the fast path      (CAS visible: ts-1 -> ts)
visible == ts, but the true contiguous frontier is ts+5
```

If no further commit arrives, `ts+1 … ts+5` stay **invisible for ever**, while their writes
are durable and acknowledged. That is precisely the rmp #2309 stall class, and out-of-order
completion under concurrency is the normal case, not a corner.

Note what kind of bug this is: it does not slow anything down. No throughput benchmark
would reveal it. It silently loses the visibility of committed work — the same shape as the
defect this sprint retracted once already, and the same shape as #2309.

## The guard must be TWO conditions

```go
if c.log.pending() == 0 && c.visible.CompareAndSwap(ts-1, ts) {
    // exact: with nothing pending above ts, advance() from ts would stop at ts+1 anyway
    ...wake waiters...
    return
}
// else: the existing locked path, unchanged
```

`pending` is the count of set bits **above** `oldest`, maintained by the log:

- `finish(ts)` with `ts > l.oldest` → `pending++` (this bit sits above the frontier);
- `advance()` consumes bits from `oldest` forward → `pending -= (newOldest - oldOldest - 1)`,
  since the first timestamp consumed is the one just finished and was never counted.

With `pending == 0` the fast path is **exact**, because `advance()` from `ts` would stop at
`ts+1` regardless.

## What still needs care before implementing

The remaining risk is **not** the CAS; it is keeping the log consistent once `visible` can
move without the lock:

- The slow path must derive `oldest = visible + 1` on entry, or it walks from a stale
  position and computes a frontier *below* `visible`. The existing
  `advanced := frontier > c.visible.Load()` guard prevents that from moving the frontier
  backwards, but the log's own state would drift.
- **Block management is the sharp edge.** `l.headStart`, `blockFor` and `retireHead` are all
  keyed off `oldest`. `advance()` retires blocks as it crosses them; jumping `oldest`
  forward directly does not. Any derivation of `oldest` from `visible` must retire the
  blocks it skips, or `blockFor` addresses a block that should have been freed.
- `finish`'s discard rule (`ts < l.oldest` returns early) stays correct under a derived
  `oldest`: a timestamp already covered by the frontier needs no bit.

## Why this task is the highest-value one left

Three contention mechanisms were examined in sprint 336 and **all three came back neutral
or negative**:

| mechanism | outcome |
|---|---|
| duplicated `UNIQUE` registry lock (#2358) | removed; no latency change |
| doubled mapper shard acquisition (#2360) | removed; geomean −0.28% |
| graph-level `writeCtx` free slot (#2361) | removal **reverted** — it upheld an allocation invariant |

And `docs/benchmarks/mvcc-contention-arms-2026-08-08.md` shows the ≈2× ceiling is reached at
**four** writers and unmoved at 16 or 32.

So the deficit is not an accumulation of per-write traffic on already-sharded state.
`pubMu` is the last genuinely **serialising** structure: one mutex, taken once per commit, by
every writer. It is the only remaining candidate that fits the measured signature.

## Prior art for the shape

- **InnoDB** `Link_buf` (`storage/innobase/include/link_buf.h`) makes the equivalent
  advance lock-free, at the cost of a pointer chase. The task explicitly defers that
  reshaping: its advantage is window-size independence, which only matters if the in-flight
  window is large — and the fast path removes most of the traffic that would make it matter.
- **PostgreSQL**'s group-clear on `ProcArrayEndTransaction` batches the equivalent work
  under one acquisition instead of eliminating it. Worth considering only *after* the fast
  path, since it addresses the same traffic a different way.
