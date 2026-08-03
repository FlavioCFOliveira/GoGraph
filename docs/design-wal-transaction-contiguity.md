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
| 4 | `txnSeq` durable and monotone across close/reopen | **DONE** — see §5 |
| 5 | crash-injection battery with concurrent writers | **DONE** — see §7 |
| 6 | benchmark showing fsyncs/commit falls as concurrency rises | **DONE** — measured, see §8 |
| E21 | recovery and bulkimport open the adjacency commit window with no barrier, licensed only by a comment | **DONE** — see §6 |

---

## 5. The transaction sequence across a reopen (criterion 4)

`txnSeq` was decoded by recovery and **never written back**, so a store reopened on a non-empty WAL
restarted at 0 and minted 1 again. One log could then hold two different transactions under one
sequence number — and that number is exactly what recovery's suffix filter uses to tell one
transaction's frames from another's. It tolerated the collision only because frame contiguity plus
equality happened to disambiguate it: an accident, not a guarantee, and one that stops holding the
moment a reopen follows a torn tail.

**Derived, not persisted.** `recovery.Result.MaxTxnSeq` reports the highest sequence any replayed v3
frame carried; `txn.Options.ResumeTxnSeq` seeds the store from it. The WAL already records the
sequence in every frame, so a separate durable counter would be a second source of truth that can
disagree with the log after a torn tail — the same reasoning rmp #2309 applies to the MVCC clock.

`MaxTxnSeq` counts frames the replay goes on to **discard**, and frames of an **incomplete tail
transaction**, not just committed ones. A sequence that was minted is spent: re-minting it would put
an abandoned transaction and a live one under one number, which is precisely the ambiguity being
closed. Pinned by `TestRecovery_MaxTxnSeqCountsAnIncompleteTail`.

Measured:

| arm | commit-marker sequences |
|---|---|
| seeded from `MaxTxnSeq` | `[1 2 3 4]` |
| unseeded (the pre-change behaviour) | `[1 2 1 2]` |

### A deadlock the partial fix introduced

Seeding the minting counter **alone** wedges the store. `waitApplyTurn` parks until
`appliedSeq == seq-1`, and the predecessor of a resumed store's first transaction was applied by the
*previous* store instance, which no longer exists to advance anything. With only `txnSeq` seeded,
`TestResumeTxnSeq_IsMonotoneAcrossReopen` **hung**: the first commit after the resume waited forever
for a sequence nobody would ever complete.

Both watermarks are seeded now. This is worth recording rather than quietly fixing, because the
obvious half of the change is the half that hangs — and it would have hung in production on the
first write after every restart.

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


---

## 6. The exclusive-build window, enforced (audit finding E21)

`AdjList.BeginCommit` and `AdjList.BeginExclusiveBuild` mutate the same two plain fields —
`bulkOwner` and `bulkDepth`. They differ **entirely in what licenses them**:

| caller | licence |
|---|---|
| the serving write path (`lpg.ApplyAtomically`, `lpg.LockBarrier`) | the graph's exclusive visibility barrier is held for the whole window |
| `store/recovery`, `store/bulkimport` | **no barrier at all** — the graph is not reachable by anyone yet: single-threaded replay, no concurrent reader, no concurrent writer |

Until this change **both called `BeginCommit`**, so the second licence lived only in a comment. The
audit's point is that it must not be silently *inherited* once writers overlap at serving time: a
path that legitimately needs no barrier during a rebuild must not quietly become a path that needs
none while the engine serves.

Two distinct entry points now, plus an `atomic.Bool` that makes the **one sound direction** panic:
an exclusive build may not START inside a serving window, because a rebuild may only run on a graph
nobody is serving. Pinned by `TestExclusiveBuild_RefusesEntryInsideAServingWindow`, which fails by
construction if the guard is removed.

### Only one direction is asserted — measured, after getting it wrong

The first version *also* panicked whenever a serving window was opened **during** a rebuild. That is
too strict, and `make ci` said so — three packages failed (`cypher`,
`examples/04_persistence`, `examples/24_social_network_cli`) because recovery's own replay nests one,
on the **same goroutine**, on the dominant path:

```
adjlist.BeginCommit
lpg.ApplyAtomically              (lpg.go:712)
lpg.reclaimAfterDirectWrite      (mvcc_gc.go:135)
lpg.addNodeInfo                  (lpg.go:1206)
recovery.applyOpCodec            (recovery.go:1616)
```

A replay creates versions fast enough to cross the reclamation threshold, and the sweep runs inside
an `ApplyAtomically` bracket. The guard was rejecting correct behaviour.

**The hazard is CONCURRENCY, not nesting** — a *second goroutine* writing while the rebuild runs.
Telling the two apart needs goroutine identity, which `adjlist` does not have: the only structure
that knows which goroutine holds a write window is lpg's `barrierGuard`, and it is
`//go:build race || gograph_debug`. So the assertion in that direction belongs in **lpg**, alongside
that guard, and is rmp #2304's to add when it retires the serving path's window. Until then the
nesting is **counted** (`AdjList.NestedServingWindows`), so it is observable rather than merely
tolerated — and it is load-bearing: if it ever stopped happening, the reclamation debt would
accumulate through a whole replay with nothing draining it.

`TestExclusiveBuild_AllowsAServingWindowNestedInside` now pins the behaviour the wrong guard
rejected, so it cannot be re-added.

### A hazard for rmp #2304 that this surfaced

`AdjList.builderOwner` prefers `bulkOwner` **over** the writing transaction's own record —
deliberately, so a window's token cannot change mid-window (the record is allocated lazily by the
first version, so reading it first would re-clone the builder on every write; a test caught that on
the first draft of rmp #2301).

The consequence is that **the serving path's window currently SHADOWS per-transaction ownership.**
With `visMu` gone, two concurrent writers would both present the same `bulkOwner` and would reuse
each other's private, *unpublished* shard builders — one writer mutating another's slot array in
place. ### Half of that hazard is closed now

`builderOwner` was reordered: the **writing transaction wins** over the bulk window's token.

The old order existed for a real reason — the commit *record* is allocated lazily, so reading it
first could yield nil on a bracket's first write and a record on its second, changing the token
mid-window and re-cloning the builder every write (a test caught exactly that on rmp #2301's first
draft). `mvcc.WriteStamp.OpenTxID` closes that gap: the id is stored by `TxState.Arm` when the window
opens, **before any write can happen**, because rmp #2299 minted it eagerly so the writer could read
through it. It is stable for the whole transaction where the record is not.

So while a transaction is open, `bulkOwner` is never consulted, and the shared token can no longer
decide who may mutate a shard's private slot array. What remains for rmp #2304 is one problem
instead of two: the **lookup** still resolves through `WriteStamp`'s single ambient slot and must
become a parameter that travels with the write — the same ambient-versus-threaded distinction
`graph/lpg/mvcc_writectx.go` already documents for conflict detection.

**No single-writer test can gate this change, and that was checked rather than assumed.** With the
old ordering restored, `TestBuilderOwner_TransactionKeepsCloneOnceWithNoBulkWindow` still passes:
the first adjacency write allocates the record itself, so the nil window the old order guarded
against never opens. For one writer the two orderings are indistinguishable — which is the claim the
change makes — and the difference appears only with two concurrent writers, which the barrier still
prevents. The test is therefore a guard on the property #2304 depends on, not a discriminator, and
it says so.

---

## 7. Crash injection with concurrent writers (criterion 5)

Every other scenario in `internal/crashinject` crashes a **single-threaded** child and asserts one
hand-computed graph shape. That cannot be done here: which transactions had committed when the kill
landed is up to the scheduler, so there is no single post-crash shape to write down.

What is not up to the scheduler is the contract. So the child **announces every acknowledged commit
on stdout** — one `ACK <id>` line, written only after `Commit` returned `nil`, i.e. after that
transaction's frames and its `OpCommit` marker were fsynced — and the parent holds recovery to two
properties over the surviving artefacts:

- **Durability** — every acknowledged transaction is present after recovery, complete.
- **Atomicity** — every transaction present after recovery is present in **full**. A transaction
  that contributed some of its five facts and not others is a violation whether or not it was ever
  acknowledged.

Plus two closing conditions: recovery **invents nothing** (every node and arc in the graph is
accounted for by some transaction in the universe), and the crash landed **after real acknowledged
work**.

**The workload.** Each transaction id owns a disjoint 3-node ring (`base = id * 10`) and commits six
ops: three arcs with id-derived weights, one label, one property. The stride is what makes
per-transaction completeness checkable *independently* — no transaction can supply or hide another's
nodes — which is the design decision that makes a concurrent crash assertable at all. Eight writers
drive 200 transactions each, after a four-transaction sequential warm-up.

**The two crash points**, both new and both elided to nothing without the `gograph_crashinject` tag:

| breakpoint | where | what it tears |
|---|---|---|
| `wal.appendrun.frame-emitted` | inside `AppendRun`'s emit, after a frame lands | one transaction's frame run, with `w.mu` held and every other writer queued behind it |
| `wal.sync.pre-datasync` | in `leadGroupSyncLocked`, after `Flush`, before `dataSync` | a group commit: the leader's suffix is in the OS but nothing below it is durable, and every follower is parked on a watermark that will never be published |

### The countdown, and why it is not a convenience

A breakpoint on a hot durability path is reached by the process's **very first commit**, and killing
there proves nothing: nothing has been acknowledged, so "every acknowledged transaction survived"
holds vacuously. `GOGRAPH_CRASH_AFTER=n` lets the first *n* hits through and kills on the (n+1)th,
moving the crash into the steady state where several writers are genuinely in flight.

It makes the crash deterministic in **count**, not in interleaving — which is exactly why the oracle
is an invariant over the child's own acknowledgements rather than one fixed shape.

Prior art: SQLite's OOM simulator counts down to its injected failure the same way —
`memfault.iCountdown`, *"Number of pending successes before a failure"*, decremented in
`faultsimStep()` and configured by `faultsimConfig(nDelay, nRepeat)`
(`sqlite/sqlite` @ `1b08739`, `src/test_malloc.c:27,65-71,119-120`). Its off-by-one is the whole
contract, so both sides are pinned by `TestBreakpoint_Countdown`.

### The instrument was verified to fail, on both arms

A checker that has never been observed to fail is not evidence.

- **Unit arm** (`TestConcurrentWritersOracle_ReportsViolations`) — hand-built graphs, no subprocess.
  A partial transaction is reported as an atomicity violation; an acknowledged transaction that
  vanished entirely as a durability violation; an invented node as a surplus; and a legitimately
  absent *unacknowledged* transaction as **nothing**, because flagging that would fail every crash
  test.
- **End-to-end arm** — with `Tx.Commit` altered to acknowledge *without* calling `SyncGroup`, both
  crash tests fail with **44 durability violations** naming the exact acknowledged-but-lost
  transactions, including all four warm-up ids. Reverted after measuring.

### What this battery does NOT yet detect, and why that is worth stating

It does **not** detect finding E5 itself. Replacing `AppendRun` with a loop of `Append` and re-running
these tests leaves them **passing**, because `appendOnly` is still called with the store's
single-writer semaphore held: contiguity is currently **over-determined**, guaranteed twice, and
removing either guarantee alone changes nothing observable.

E5's detector is therefore the `store/wal` unit arm (criteria 1–3), which drives concurrent appenders
against the writer directly and measures the run count — a loop of `Append` shatters a 25-frame
transaction into **8 runs**, `AppendRun` gives exactly **1**.

This battery becomes the E5 gate the moment rmp #2306 retires the semaphore. Until then its value is
what the unit arm cannot reach: the **atomicity and durability of a real store, recovered from a real
`kill -9`, with many transactions genuinely in flight**.

A caveat stated rather than buried: `SIGKILL` does not drop the OS page cache, so the mid-fsync point
tests recovery over a **never-fsynced tail**, not the **loss** of one. Losing the tail outright is
what `internal/sim`'s fsync-fault injection covers.

---

## 8. Group commit still coalesces, measured (criterion 6)

`BenchmarkWriteScaling_StoreAPI` (`store/txn/write_scaling_bench_test.go`) reports `commits/fsync`
directly. Apple M4, 10 cores, `-benchtime 3000x -count 5`, no `-race`, quiet machine; median of five:

| writers | commits/fsync | **fsyncs/commit** | ops/sec |
|--:|--:|--:|--:|
| 1 | 1.000 | 1.000 | 263 |
| 8 | 4.121 | 0.243 | 1 073 |
| 64 | 31.58 | 0.032 | 8 472 |
| 256 | 107.1 | 0.0093 | 28 530 |
| 1024 | 300.0 | 0.0033 | 78 667 |

fsyncs per commit falls **300×** from one writer to 1024, and throughput rises **299×**. The
criterion asks for the curve, and the curve is monotone across every step.

The contiguity change did not cost this: `AppendRun` holds the writer's mutex for the append only,
and `SyncGroup` coalesces on **Sync**, not on Append, so a run of appends followed by one sync is
precisely the shape it already batched.

The number to keep in view is that this is the **store API** path. The Cypher path is flat at
**268 commits/s** at every writer count, because it fsyncs inside `visMu` — the same 268 the audit
recorded, and rmp #2304's to remove. Two paths over one WAL, one of which coalesces 300× and one of
which cannot coalesce at all, is the sprint's thesis in a single table.
