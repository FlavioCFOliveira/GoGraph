# Design: withdrawing an aborted transaction

**Task rmp #2318** · sprint 334 · 2026-08-04

Status: **implemented.** `graph/lpg/mvcc_abort_reclaim.go`,
`graph/lpg/mvcc_abort_sides.go`, `graph/mvcc/conflict.go`.

---

## 1. Two defects, and only one of them was in the ticket

**The leak (the ticket's).** `mvcc.AbortedTS` is `^uint64(0)`, the maximum uint64,
and every reclaimer truncates on `stamp <= watermark`, which it can never satisfy.
Measured at `b3e1aa0b`: seed 50 nodes with a label, reclaim to zero, write a label
on each of the 50 inside a transaction, abort, reclaim again with no live reader —
`freed=0`, `VersionCount()=50`, for the life of the process. Live since `d8847ce7`
made a write-write conflict abort the transaction, so every serialization failure
leaked.

**The atomicity violation (found while fixing the leak).** The stored value keeps
the aborted transaction's writes; its deltas are the only mask. Two ways that
surfaces:

```
(a) T_abort adds label L to n, aborts.  Stored = {…, L}; head delta is aborted.
    A PRESENT-TIME read takes the stored value directly — ReadAt(nil) is
    documented as "the current stored value", which every plain getter uses.
    Measured: HasNodeLabel(n, "L") == true, immediately after the abort.

(b) T2 adds M and commits. Conflicts() exempted an aborted head, so T2 built its
    value from the dirty bag: {…, L, M}. A reader after T2 walks the chain, finds
    T2's delta VISIBLE, and BREAKS — never reaching the aborted delta behind it.
    Measured: `reader sees L=true M=true`.
```

Both are committed reads observing work from a transaction that was told it
failed — Atomicity, not memory. The chain walk's early break is sound only while a
chain is **timestamp-monotone**; an aborted delta breaks that, being the maximum
uint64 and yet invisible to everyone.

## 2. Why the ticket's proposed shape does not work

The ticket proposed doing the withdrawal in the background vacuum (rmp #2308).
That closes the leak and **not** case (a): a present-time read takes the stored
value, so between the abort and the sweep the aborted writes are readable. An ACID
property cannot depend on when a goroutine runs.

Nor does "make every walk continue past a visible delta" fix case (b). It would fix
the label, property and edge-side stores, whose deltas are undo **actions** that
compose in any order. It cannot fix the **adjacency**, whose chain holds immutable
entry **snapshots**: T2 built its entry from the dirty base, so T2's entry itself
contains the aborted edge. No walk recovers a value that was never recorded.

## 3. What the references actually do

The ticket cites both, and misdescribes both.

- **Memgraph** withdraws **at abort**, not in its GC, and says so in its own
  source: *"Abort will modify objects to restore state to how they were before this
  txn"* (`InMemoryStorage::InMemoryAccessor::Abort`,
  `src/storage/v2/inmemory/storage.cpp:1482-1790`, read 2026-08-04 at commit
  `0e8aa326`). It restores each object under that object's lock, unlinks the
  deltas, and only then hands them to `garbage_undo_buffers_` with a
  `mark_timestamp` so the GC frees the **memory** once
  `mark_timestamp <= oldest_active_start_timestamp` (`:1792-1808`, and `:3084-3100`
  for the GC side). Its GC never applies an undo.
- **PostgreSQL** has no undo log at all. An aborted transaction's tuple version is
  simply never visible (its `xmin` is an aborted xid per the CLOG) and the previous
  version was never overwritten, because PostgreSQL appends a new tuple version
  rather than mutating in place. VACUUM reclaims the dead tuple and undoes nothing.

GoGraph mutates the stored value in place with undo records beside it, which puts
it in Memgraph's family and not PostgreSQL's. So Memgraph's timing — **at abort** —
is the one that fits.

## 4. The two halves shipped

**Half 1 — the withdrawal is synchronous, at abort.** `Graph.abortWake` calls
`Graph.withdrawAbortedNow` before returning: it takes the single-sweeper slot and
restores every store that carries an aborted head, computing each clean value
through **that store's own `asOf` walk** — the same code a reader runs, so the
withdrawn value cannot disagree with what readers have been seeing. Covered:
node labels, node properties, the five per-edge side stores (one generic
`sideVersions.withdrawAborted`), node-existence records with their tombstone-bitmap
reconciliation, the adjacency conflict stamps, and the deferred label-index
removals.

**Half 2 — an aborted head conflicts.** `mvcc.Conflicts` no longer exempts
`AbortedTS`, so it is once again the plain negation of `Visible`. This covers the
race window between `info.Abort()` and the withdrawal completing, where a
concurrent writer would otherwise build on the dirty base. It **reverses rmp
#2300**, whose exemption existed because refusing made the first aborted-on object
"permanently unwritable" — measured then, with `examples/27_concurrent_txn`'s
writers exhausting a nine-attempt retry chain. That was true *with no cleaner*.
With Half 1 the head is gone before the next writer arrives, so the cost is a
transient retriable serialization failure, which is already this sprint's contract.

Neither half works alone: Half 1 alone leaves the race window open, and Half 2
alone is the livelock rmp #2300 measured.

## 5. The cost, stated

The withdrawal scans the objects **carrying history**, not the objects the
transaction touched, because the substrate keeps no per-transaction write set —
Memgraph's `transaction_.deltas`. Adding one means an append on every write to
serve the rare abort path, on a sprint whose objective is write throughput. The
scan is therefore O(objects carrying history), bounded by the retained version
count and so by `reclaimDebtCeiling` plus whatever a live reader holds back. Making
it O(the transaction's own writes) needs the write set and is a separate change.

## 6. One correction found by an absolute oracle

Withdrawing a label the aborted transaction **added** must also remove its
label-index entry. Adds go into the bitmap immediately — only removals are
deferred — so leaving the entry makes the bitmap over-report, which is normally the
harmless direction and here is not: `Graph.LabelCountExact` serves `count(*)` from
the bitmap whenever nothing is deferred, and the withdrawal has just cleared the
aborted transaction's deferrals. Measured before the correction:
`MATCH (n:Person:Admin) RETURN count(*)` answered **1** against a hand-computed
oracle of **0** (`TestMVCCSnapshotRead_AbsoluteOracle`). Only labels the withdrawal
actually took away are removed, and only when the clean bag no longer carries
them — a label still held from an earlier committed add keeps its entry, because
losing it is the unrecoverable direction.

## 7. Tests

| Test | What it pins |
|---|---|
| `TestAbort_VersionsAreReleasedBySweep` | The ticket's reproduction: 50 aborted versions, zero retained afterwards, and a later sweep finds nothing left. |
| `TestAbort_WithdrawnWritesStayInvisible` | The stored value is clean immediately after the abort, and to a reader starting later — so the fix is a withdrawal and not a removal of the mask. |
| `TestAbort_StoredValueEqualsTheSerialSchedule` | Every touched store compared against a **control graph** driven through the identical committed writes with the aborted transaction omitted, so the oracle cannot inherit the code's mistake. |
| `TestAbort_DirtyBaseIsNotWritable` | Liveness: the object is writable once withdrawn, which is what rmp #2300's exemption existed to guarantee. |
| `TestAbort_VersionsAreWithdrawnAtAbort` | The **inversion** of `TestAbort_VersionsAreNotYetReclaimable_2318`, whose own failure message asked for exactly this. |
| `TestConflicts_TheFourCases` (`graph/mvcc`) | The reversed contract, with both measurements recorded in the failure text. |
| `TestLabelTx_ComposesWithPhysicalUndo` | Updated from "2 deltas retained" to zero: the composition's cost is now nothing rather than twice the deltas. |
