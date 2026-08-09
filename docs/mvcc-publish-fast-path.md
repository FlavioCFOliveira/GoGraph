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

## A third subtlety: the `pending` check and the CAS are not atomic together

The two-condition guard above is necessary but **not sufficient as written**, and this is the
last thing to get right.

`pending` can go 0 → 1 between the check and the CAS. If a slow-path `finish` sets a bit
*above* the frontier in that window, the fast path still advances `visible` by one and leaves
that bit behind. If no further commit arrives, it is the same stall — reached through a race
rather than through ordinary ordering.

The invariant that makes the rest of the design work is: **the fast path only runs when
`pending == 0`, so there are never set bits above the frontier while it advances** — which is
also what lets the slow path fast-forward `oldest` from `visible` without skipping a recorded
bit. The race above is the one hole in it.

Closing it is cheap: **re-check `pending` after the CAS**, and if it is non-zero fall through
to the locked path so `advance()` can catch up.

```go
if c.log.pending.Load() == 0 && c.visible.CompareAndSwap(ts-1, ts) {
    if c.log.pending.Load() == 0 {
        ...wake waiters...
        return
    }
    // a bit landed above us mid-CAS: fall through and let advance() catch up
}
```

But note the consequence for the locked path: `ts` is **already** reflected in `visible`, so
`finish(ts)` will take its `ts < l.oldest` early return and never call `advance()`. The
fall-through therefore needs a *catch-up* entry point — sync `oldest` from `visible`, then
`advance()` — rather than a plain `finish(ts)`. That is the piece to write, and
`TestClock_Frontier*` in `graph/mvcc/frontier_liveness_test.go` is the oracle it must satisfy;
those tests already fail on the naive one-condition version at a window of **two**.

---

# What was implemented, and the fourth subtlety the code found

Everything above is the analysis that preceded the code. The code then found one more hole,
which is why the guard that shipped is not the one written above.

## A fourth subtlety: `pending == 0` is not enough even WITH the re-check

The guard above counts only *set bits above the frontier*. That is necessary and still not
sufficient, and the failing interleaving needs no unusual timing at all:

```
frontier f; commits f+1 (in order) and f+2 (out of order) both in flight

B (f+2)  takes pubMu, reads the frontier: f
A (f+1)  pending == 0 (B has set no bit yet) -> CAS f -> f+1 -> re-check: still 0 -> RETURNS
B        records f+2 above f, computes its frontier from what it read: f
B        installs nothing, because f is not above the f+1 A just published
=> frontier f+1, and commit f+2 is durable, acknowledged, and invisible for ever
```

A's re-check is honest — nothing was pending when it looked. The bit arrives *after*. So the
guard must also be raised while a publisher is **inside** the locked path, from before it
reads the frontier until after it has installed one:

```
blocked != 0  ⟺  a publisher is inside the locked path  OR  a bit sits above the frontier
```

Both halves are load-bearing, and the proof that together they are sufficient is short.
A stall needs a fast path whose re-check `Q` reads zero and a publisher `B` that read the
frontier before that fast path's CAS `P` and stranded a bit. In program order B does
`enter < read < bit < exit`, and `read < P < Q`. For `Q` to read zero, B's `exit` must
precede `Q`; but `bit < exit < Q` means the stranded bit was already counted when `Q`
looked, so `Q` cannot have read zero. Contradiction. ∎

Both halves were then verified **by injection**, not by the argument alone. Against
`TestClock_FrontierSurvivesTheFastPathLockedPathRace`, deleting the publisher bracket fails
at rounds 348 and 10 406; deleting the post-CAS re-check fails at 3 407, 12 813, 15 605,
17 792 and 23 282. The test runs 100 000 rounds, sized to the worst of those.

## The shape that shipped

- `commitLog.blocked` is an atomic **flag**, not a count. The fast path only compares it
  with zero, so the number of reasons interests no reader; publishing every reason cost
  measurably more (see below). The real count lives in `commitLog.bits`, plain arithmetic
  under `pubMu`, and `blocked` is stored only when the safe/unsafe transition happens.
- `commitLog.syncTo(floor)` is the catch-up entry point. It declares everything at or below
  `floor` finished, retires the blocks the jump skips (`retireBehind`), and then advances
  over anything already recorded above. The locked path calls it **unconditionally**, before
  `finish`.
- One read of `visible` serves both `syncTo`'s floor and the install; the install is a
  compare-and-swap loop (`raiseVisibleFrom`) rather than a store, because the fast path can
  now raise the frontier without the lock and a plain store could land an older value on top
  of a newer one.

### One shortcut that looked safe and is not

"While the flag is up, no fast path can complete, so nothing but a locked publisher can
raise the frontier, so `syncTo` can be skipped." **False.** A fast path raises `visible`
with its CAS and only *then* sees the flag on its re-check, so a raise can and does land
while the flag is up. `syncTo` is unconditional for that reason, and the reason is recorded
on `enterPublish` so it is not re-derived and re-adopted.

## Measured

`docs/benchmarks/mvcc-publish-fastpath-2026-08-09.md`, interleaved with a byte-identical
self-control in the same loop, n = 10, noise floor ≈1%:

- End-to-end on rmp #2359's arms: **every significant row is a win, none is a regression**.
  `label-add-remove` −3.58%/−3.55%/−3.69% at 8/16/32 writers; `update-property` and
  `create-labelled-node` ≈ −1.2%; one writer flat everywhere.
- The win tracks **publications per unit of work**, which is the mechanism claimed:
  `label-add-remove` is two statements, hence two commits, and gains twice what the
  single-statement arms gain.
- Mechanism level: in-order publication **−54.6%** (6.08 ns → 2.76 ns). The synthetic
  reverse-window arm, in which the fast path never fires once, is **+98.6%** — recorded as a
  real regression, and one that no end-to-end arm pays.

The prediction in this document was "at most ~2.5%". The measured end-to-end win exceeds it
on the arm with the most commits and falls short of it on the others.
