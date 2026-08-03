# Keeping a transaction's WAL frames contiguous under concurrent appenders

**rmp #2302 (MVCC A5) · sprint 334 · 2026-08-03**

Audit: [`audit-mvcc-sole-cc-2026-08-02.md`](audit-mvcc-sole-cc-2026-08-02.md), finding **E5** —
"the most dangerous item in the whole sprint because its only symptom is a counter".

---

## 1. The defect

Recovery buffers a v3 transaction's ops until its `OpCommit` marker arrives, then commits **the
contiguous suffix** whose `TxnSeq` matches the marker's
(`store/recovery/recovery.go:1421-1429`):

```go
commitSeq := op.TxnSeq
start := 0
for start < len(pending) && pending[start].TxnSeq != commitSeq {
	start++
}
if start > 0 {
	metrics.IncCounter("store.recovery.openCodec.orphanedOps", uint64(start))
}
committed := pending[start:]
```

The prefix is discarded as orphaned — ops of a prior transaction whose marker was never written.
That reading is correct **only because commits are serialised**, and the comment says so in its own
words: *"The store serialises commits (single writer), so a transaction's frames are contiguous and
never interleave with another's."*

Where does that serialisation come from? Not from the WAL. `wal.Writer` documents itself as *"safe
for concurrent calls to Append / Sync / Stats; all mutations serialise on an internal mutex"* — it
serialises **individual appends**, not runs of them. Contiguity comes solely from
`store/txn/txn.go` holding the store's single-writer semaphore across the whole append loop.

So the moment two writers append concurrently, frames interleave — and the damage depends on
**where** the foreign frame lands. Both orders occur, both were measured, and they break *different*
ACID properties.

### Order 1 — the foreign op lands in the buffer's PREFIX: committed data is LOST

```
b1(seq2)  a1(seq1)  a2(seq1)  commit(seq1)  b2(seq2)  commit(seq2)
```

On `commit(seq1)` the buffer is `[b1(2), a1(1), a2(1)]`. The scan walks past index 0 to find
`seq1`, so `start == 1` and **`b1` is discarded as an orphan**. The buffer then resets, so `b1` is
never seen again: transaction 2 commits with only half its ops.

Measured (`TestRecovery_InterleavedTransactionsDropCommittedOps`):

```
present=map[a1:true a2:true b1:false b2:true]  orphanedOps=1
```

Three of four ops are there and the graph looks entirely plausible. **That is what makes it
dangerous** — the only signal is one counter. A **Durability** violation.

### Order 2 — the foreign op lands in the MIDDLE: a phantom partial commit

```
a1(seq1)  b1(seq2)  a2(seq1)  commit(seq1)   ← crash here
```

Now the scan finds `seq1` at index 0, so `start == 0` and **all three are applied** — `b1` becomes
durable under transaction 1's marker, before transaction 2 committed anything. Crash between the
two markers and `b1` is durable with no commit behind it.

Measured (`TestRecovery_InterleavedTransactionsCommitAnotherTransactionEarly`):

```
present=map[a1:true a2:true b1:true]  orphanedOps=0
```

Zero orphans, nothing dropped, and an **Atomicity** violation instead: an uncommitted
transaction's write survived a crash.

> **A premise this design got wrong, corrected by measurement.** The first draft asserted that the
> order-2 interleaving loses data. It does not — it recovers all four nodes with zero orphans. Only
> order 1 loses data, and order 2 breaks a different property entirely. Writing the test against the
> assumed order produced a green result over a live defect; the two arms above exist because the
> assumption was wrong, not because both were foreseen.

---

## 2. The three options, weighed

| | scheme | prior art | verdict |
|---|---|---|---|
| (a) | reserve a contiguous byte range per transaction; appenders fill their own range concurrently | PostgreSQL `XLogInsertRecord` reserves space under `insertpos_lck`, copies the record outside it | **rejected for now** — it restructures `wal.Writer` around reserved-but-unwritten holes, which means recovery must tolerate a torn hole in the middle of the log. Its payoff is *concurrent memcpy*, a throughput win GoGraph has not measured a need for. Revisit only if the WAL append becomes the measured bottleneck; today it is not (the audit measured WAL commits flat at 268/s for a different reason). |
| (b) | keep frames interleaved; make recovery group by transaction id | InnoDB redo groups by mini-transaction | **rejected** — it is cheap to implement (the frames already carry `TxnSeq`, so `pending` becomes `map[seq][]Op`) but it makes recovery's buffer bound *per transaction times concurrency*, so an aggregate cap has to be invented on top of the existing `maxTxnOps`, and it leaves the WAL a structure whose meaning depends on a reader that reassembles it. |
| (c) | **append the transaction's frames as one run, under one acquisition of the WAL's own mutex** | PostgreSQL's *insight*, at the right granularity: do the expensive work outside the lock, hold it only for the copy | **CHOSEN** |

### Why (c)

1. **Contiguity becomes a property of the WAL writer, not of a lock two layers above it.**
   Recovery's assumption stays *true* instead of being relaxed — no change to
   `store/recovery`, no change to the on-disk format, no new field, no compatibility question.
   A durability invariant that the component owning the file enforces itself is worth more than
   the same invariant enforced by a semaphore in another package.
2. **It is strictly less exclusion than today.** The store semaphore currently spans encode **and**
   append **and** everything else a commit does. `AppendRun` holds only `Writer.mu`, and only for
   the framing. Every other writer can proceed with everything except its own WAL append.
3. **It costs no memory.** The alternative shape — encode every op into a staging buffer, then hand
   `[][]byte` to a batch append — would hold the whole encoded transaction at once. The callback
   form keeps `store/txn`'s pooled single scratch buffer, which #2289-era tuning established and
   which the alloc gates watch.
4. **Group commit is untouched.** `SyncGroup`'s leader/follower coalescing
   (`store/wal/writer.go`) operates on `Sync`, not on `Append`; a run of appends followed by one
   Sync is exactly the shape it already coalesces.

### The shape

```go
// AppendRun appends every frame the callback emits as ONE contiguous run.
func (w *Writer) AppendRun(fn func(emit func([]byte) error) error) error
```

`w.mu` is taken once, before `fn`, and released after it. The `emit` closure handed to `fn` is
valid only for the duration of the call and must not be retained. `fn` must not call back into the
Writer — the mutex is not re-entrant — which is stated on the method and is the one hazard the
callback form introduces.

---

## 3. What this increment delivers, and what it does not

rmp #2302 carries seven acceptance criteria. This increment closes the ones about contiguity; the
rest are separable and are recorded here rather than implied.

| # | criterion | status |
|---|---|---|
| 1 | concurrent commits recover completely, no partial ones | **DONE** |
| 2 | that test verified to FAIL against the contiguity assumption | **DONE** — see §4 |
| 3 | the orphan-discard counter is zero under legitimate concurrent commits | **DONE** |
| 7 | the chosen scheme cites project, file and symbol | **DONE** — §2 |
| 4 | `txnSeq` durable and monotone across close/reopen | **NOT YET** |
| 5 | crash-injection battery with concurrent writers | **NOT YET** |
| 6 | benchmark showing fsyncs/commit falls as concurrency rises | **NOT YET** |
| E21 | recovery and bulkimport open the adjacency commit window with no barrier, licensed only by a comment | **NOT YET** |

---

## 4. The instrument, validated against the defect

A test that only exercises the fixed code proves nothing. `TestAppendRun_KeepsATransactionContiguous`
interleaves two transactions' appends deterministically and then recovers:

**At the WAL layer** (`store/wal/append_run_test.go`), one transaction of 25 frames is appended
while 8 goroutines append their own as fast as they can:

| arm | result |
|---|---|
| `AppendRun` | **1 run**, all 25 frames adjacent |
| a loop of `Append` | **8 runs**, longest 10 of 25 — the transaction shattered into 8 fragments |

**At the recovery layer** (`store/recovery/interleaved_txn_test.go`), the same two transactions in
three frame orders: contiguous (all four ops recovered, `orphanedOps=0`), prefix-interleaved (one
committed op lost, `orphanedOps=1`), middle-interleaved (an uncommitted transaction's op durable
after a crash).

The losing arms are the point. They stay in the tests permanently, not deleted after the fix,
because the failure mode is silent and the next person to touch the append path needs to see what it
costs rather than read that it would. Each one also fails LOUDLY if the defect ever stops
reproducing — because that would mean the guarantee this design rests on is no longer the thing
protecting atomicity, and the positive tests would be proving nothing.
