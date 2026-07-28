# Concurrent write scaling: the apply gate's one-waiter wake — 2026-07-27 (rmp #2221)

- Apple M4 (10 cores, 32 GB), Go 1.26.5, darwin/arm64. Real on-disk fsync, `t.TempDir()` WAL.
- Harness `store/txn/write_scaling_bench_test.go`. One durable single-edge transaction per
  operation through `txn.Tx.Commit`.
- `benchstat` over `-count=6`, with a per-concurrency `-benchtime` chosen so **every writer
  performs at least ~50 operations**. This matters: at `-benchtime=3000x` and 1024 writers each
  goroutine runs only ~3 commits, so spawn and drain dominate the wall clock and the measurement
  is worthless. The exact invocations are in §4.

## 1. Result

| writers | before | after | change |
|---|---|---|---|
| 1 | 264.2 ops/s | 263.9 ops/s | ~ (p=0.734) |
| 8 | 1 054 ops/s | 1 061 ops/s | ~ (p=0.288) |
| 64 | 8 388 ops/s | 8 306 ops/s | ~ (p=0.310) |
| 256 | 31 300 ops/s | 31 630 ops/s | +1.04% (p=0.026) |
| **1024** | **7 425 ops/s** | **111 897 ops/s** | **+1407% (15.1×, p=0.002)** |

Mean group size (commits amortised per real fsync):

| writers | before | after |
|---|---|---|
| 256 | 120.3 | 121.3 |
| **1024** | **26.8** | **424.0** |

Throughput is now **monotonic in concurrency** — 264 → 1 061 → 8 306 → 31 630 → 111 897, a
**424× span from 1 to 1024 writers** — where before it peaked at 256 writers and then collapsed
3.9×. `sec/op` geomean −41.9%; ops/sec geomean +72.2%.

At 1024 writers the engine now runs at **111 897 / 424 = 264 fsync/s**, which is exactly the
single-writer fsync rate of this device (264.2 ops/s at 1 writer). **The disk is saturated**: the
commit path is at the hardware ceiling and no longer loses anything to coordination.

## 2. What was actually wrong

Round 3 measured GoGraph "flat at 261 op/s from 1 to 1024 writers" and the round-4 audit recorded
GoGraph as having no group commit at all. Both readings were incomplete. Measurement here shows:

- Group commit **already existed and already worked** on the store API path
  (`txn.Tx.Commit` → `appendOnly` releases the single-writer semaphore → `wal.Writer.SyncGroup`
  coalesces). It reached 31 300 ops/s at 256 writers before this change — 118× the single-writer
  rate.
- The flat 261 op/s belongs **exclusively to the Cypher engine path**, which fsyncs while holding
  `lpg`'s visibility barrier `visMu` in write mode (`cypher/api.go` `commitUnderBarrier` →
  `txn.Tx.CommitWALOnly`). That serialises every writer across the disk sync, so `SyncGroup` never
  has a second caller to coalesce with. It is unaffected by this change and remains open as
  **#2193** — moving that fsync requires two-phase visibility, an architectural change.
- The **real** defect was in neither place. `Tx.advanceApply` woke the sequence-ordered apply gate
  with `sync.Cond.Broadcast`, so every committed transaction woke **all** parked committers; all
  but the one holding `seq+1` re-checked `appliedSeq`, failed, and parked again. O(N) wakeups per
  commit, O(N²) per batch.

A CPU profile at 1024 writers attributed **77.09% of all samples** to `sync.(*Cond).Wait`, and
`pprof -peek` put **100% of that** under `waitApplyTurn`:

```
      flat  flat%   sum%        cum   cum%
    13.78s 44.34% 44.34%     13.79s 44.37%  runtime.pthread_cond_wait
     6.55s 21.07% 88.19%      6.57s 21.14%  runtime.pthread_cond_signal
                                            23.96s   100% |   ...txn.(*Tx[...]).waitApplyTurn
     0.05s  0.16%  0.16%     23.96s 77.09%                | sync.(*Cond).Wait
```

The herd starved the very `Append` path that fills the next group-commit batch, which is why the
group **shrank** as writers were added (120 at 256 writers, 26.8 at 1024) — the tell that separated
this cause from a coalescing failure.

## 3. The fix, and one thing that did *not* work

The apply gate is a strictly sequential chain: the holder of `seq` waits for `appliedSeq == seq-1`,
so a completing transaction has exactly **one** possible successor. `advanceApply` now wakes that
one committer through a per-sequence parking slot (`Store.applyWaiters`), making the handoff O(1)
in the number parked. An uncontended committer finds `appliedSeq == seq-1` and returns without
registering a slot at all, which is why 1–64 writers are unchanged.

**The audit's headline recommendation was measured and rejected.** The round-4 audit called Neo4j's
`TransactionLogQueue` one-waiter wake chain (`TxConsumer.complete()` unparks only the batch's first
element, which unparks the other N−1 itself) "the highest-value single thing this audit found to
take from either incumbent". It was implemented faithfully in `wal.Writer` — an intrusive FIFO of
parked committers, detached by the leader in O(1), whose head propagates the unpark — and it
**regressed throughput 3–5%**:

| writers | apply-gate fix only | + WAL one-waiter chain |
|---|---|---|
| 256 | 31 800 ops/s | 30 800 ops/s |
| 1024 | 110 900 ops/s | 105 300 ops/s |

The technique does not transfer to Go. Java pays an explicit `LockSupport.unpark` loop on the
appender thread, which is what the chain exists to avoid; Go's `sync.Cond.Broadcast` is a
`notifyListNotifyAll` list splice that hands every waiter to the runtime at once, letting it spread
the wakeups across all Ps. Replacing that with N sequential channel sends serialises on one
goroutine and is strictly worse. The WAL change was therefore **backed out**, per the mandate that
a change regressing ns/op without documented justification must not be merged. What remains is the
same structural insight applied where GoGraph's O(N) wake actually was — the apply gate — where the
successor is uniquely determined and the wake is a genuine O(1) handoff rather than a delegated
fan-out.

## 4. Reproduction

```bash
for spec in 1:2000 8:6000 64:12800 256:51200 1024:60000; do
  w=${spec%%:*}; n=${spec##*:}
  go test ./store/txn/ -run '^$' \
    -bench "BenchmarkWriteScaling_StoreAPI/writers=${w}\$" \
    -benchtime=${n}x -count=6
done
```

`BenchmarkWriteScaling_Cypher` in the same file is the control that isolates the `visMu` ceiling:
it stays at ~260 ops/s at every concurrency (260.1 / 259.6 / 259.7 / 260.0 / 259.3 at
1 / 8 / 64 / 256 / 1024), and will keep doing so until #2193 lands.

## 5. Correctness

- `go test -race ./store/... ./internal/crashinject/` green, including the WAL recovery suites and
  the deterministic crash-injection battery.
- New regression guards in `store/txn/apply_gate_test.go`: a mixed
  `Commit`/`CommitWALOnly` run at 256 concurrent writers (a lost wakeup or a gap in the sequence
  chain wedges the store, surfacing as a timeout) plus a durability replay of every acknowledged
  commit, and an assertion that a single writer never registers a parking slot. The lost-wakeup
  guard was validated by injecting the fault: with the successor's unpark removed it hangs and
  fails, as required.
