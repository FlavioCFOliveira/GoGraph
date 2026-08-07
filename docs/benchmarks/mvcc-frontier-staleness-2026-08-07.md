# Reader staleness from the contiguous commit frontier — rmp #2343 (SPIKE)

**Date:** 2026-08-07 · **Branch:** `sprint-335` · Apple M4 (10 cores), darwin/arm64

**Recommendation: ACCEPT the trade unchanged.** It is now measured rather than
assumed, and the measurement says the frontier costs the *typical* reader nothing and
the *tail* one fsync.

## The trade

A reader starts at the contiguous commit frontier — the newest instant below which
nothing is still in flight. On the durable path the commit timestamp is allocated
**before** the WAL fsync, so an fsync sits inside every allocate-to-publish window and
one slow committer holds back the visibility of every commit allocated after it,
however long those have been published.

## AC3 — can the timestamp be allocated AFTER the fsync? No, and the code says so

`store/txn/txn.go:1503` `appendOnly(commitTS)` encodes the timestamp **into** the
OpCommit marker at `txn.go:1557` (`encodeCommitV3Into(buf, seq, commitTS)`), and that
frame is what `txn.go:1387` `SyncGroup(mark)` fsyncs. The timestamp must therefore
exist before the append, and *a fortiori* before the fsync.

This is not incidental framing. Under rmp #2309 the MVCC clock is **derived from the
WAL at recovery** rather than restored from a persisted counter, so a durable record
that did not carry its instant would leave recovery unable to establish the clock
floor. Allocating after the fsync is not a tuning option; it would break recovery.

WAL frame contiguity is not the constraint — `wal.Writer.AppendRun` holds the writer's
mutex for the framing only, and recovery groups a transaction by the `TxnSeq` each
frame carries, not by adjacency. **The constraint is that the record must carry the
instant.**

## Measurement

`bench/mvccwrite/frontier_staleness_test.go` (`TestFrontierStaleness`, soak layer).
For each of 200 marker commits: the time from `Commit` returning — the client has been
told SUCCESS — to the first moment a **new** reader can see it. Each probe is a
separate `Engine.Run`, so each takes its own snapshot at the frontier as it then
stands; a long-lived reader would pin one instant and never observe the advance.

WAL wiring, because the fsync inside the window is the mechanism. Background writers
run on a disjoint key space so they never conflict with the marker committer — they
exist only to keep commits in flight.

| writers | bg commits | p50 | p90 | p99 | max |
|---:|---:|---:|---:|---:|---:|
| 1 | 360 | 46.4 µs | 117.5 µs | 323.6 µs | 922 µs |
| 2 | 469 | 42.0 µs | 72.0 µs | 161.4 µs | 4.17 ms |
| 4 | 813 | 43.3 µs | 67.8 µs | 299.5 µs | 4.04 ms |
| 8 | 1657 | 48.3 µs | 90.5 µs | **4.27 ms** | 4.44 ms |

Host fsync latency: the WAL arm of `BenchmarkWriteScaling` measures ~270 commits/s at
one writer, i.e. **~3.7 ms per commit**, essentially all of it fsync. The 4.0–4.4 ms
ceiling in the `max` column is one fsync, and it is the same order as the 3.73 ms
`graph/mvcc/commitlog.go` already quoted for the longest in-flight commit.

## AC2 — separating the frontier from inherent commit latency

The **single-writer arm is the control**: with one writer there is no earlier in-flight
commit to hold the frontier back, so its lag is inherent — the read path's own cost
plus polling granularity. The frontier's share at N writers is the excess over it.

- **Median: no share at all.** 46.4 µs at 1 writer against 48.3 µs at 8. The frontier
  adds nothing a typical reader can measure, even at 1657 background commits.
- **Tail: the whole share, and it is bounded.** p99 rises from 323.6 µs to 4.27 ms —
  a 13× inflation, of which ~3.9 ms is the frontier. That figure is **one fsync**, and
  it is the mechanism's ceiling rather than an unbounded convoy: a reader waits for the
  oldest unfinished commit, and that commit is waiting for its own fsync, not for a
  queue of them.

So the cost is not "readers are stale under load". It is **"one reader in a hundred,
at eight concurrent writers, waits one fsync"**.

## Options, ranked correct → secure → fast

1. **Accept unchanged (RECOMMENDED).** Correct today; nothing to secure; the measured
   cost is zero at the median and one fsync at p99. No read-path cost.
2. **Allocate the timestamp after the fsync.** **Rejected as incorrect** — the durable
   record must carry the instant or recovery cannot derive the clock (AC3 above). This
   is the option the ticket asked to check, and the code answers it outright.
3. **Publish out of order / non-contiguous frontier.** Rejected: `graph/mvcc/mvcc.go`
   `PublishCommitTS` documents why the frontier is contiguous — a reader handed an
   instant that includes a commit but excludes an earlier unfinished one can observe a
   state no serial order produced. That trades an isolation guarantee for tail latency.
4. **Let a reader start ABOVE the frontier and revalidate.** Costs the READ path a
   per-object revalidation on every read, to remove a tail that is already bounded by
   one fsync. Correct → secure → **fast** ranks this last: it degrades the common case
   to improve the 99th percentile.

Option 1 is recommended because 2 is incorrect, 3 weakens isolation, and 4 pays on
every read for a bounded tail.

## What this changes

Nothing in the code. What changes is that the trade is no longer unmeasured: the
figure to quote is **p50 unaffected, p99 +3.9 ms at eight writers, bounded by one
fsync**, and `graph/mvcc/commitlog.go`'s prose can be read against it.
