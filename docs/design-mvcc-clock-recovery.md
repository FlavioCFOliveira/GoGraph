# Restoring the MVCC clock on recovery — specification

rmp #2309 (MVCC C3). Status: **specified, not yet implemented.** This document is
the Specify step; it records what was verified against the code rather than
assumed, settles the on-disk compatibility question, and subdivides the work.

## The problem

`mvcc.Clock` is process-local and constructed at zero on every open
(`graph/mvcc/mvcc.go:118`). Nothing in `store/wal`, `store/recovery`,
`store/checkpoint` or `store/snapshot` records or restores a commit timestamp.
Today the consequences are bounded, because timestamps are only ever compared
within one process lifetime. They stop being bounded the moment anything needs to
name an instant durably: an MVCC-consistent checkpoint (#2310), a point-in-time or
as-of read, or replication.

## Verified against the code — three findings

These were checked, not assumed, because the task's own premises had to be
confirmed before any format decision.

**1. No on-disk format bump is needed (AC 5).** The WAL frame header is fixed at
magic + version + length + crc32c (`store/wal/format.go:32`), so it carries no
per-record shape and is unaffected. Inside the frame, `decodeV3`
(`store/recovery/recovery.go:421`) copies `payload[10:]` verbatim into `Op.Body`
after the version, kind and `txnSeq` words. `OpCommit`'s body is empty today and is
ignored by the replay state machine — its "sole effect is on the replay state
machine" (`store/txn/txn.go:178`). Therefore:

- appending 8 bytes of commit timestamp to the `OpCommit` body is **invisible to an
  older reader**, which ignores that body entirely;
- a **newer reader on an older WAL** sees an empty body, contributes nothing to the
  maximum, and falls back to the derived floor.

So neither `CurrentVersion` nor `OpRecordV3` changes, and `store/csrfile` is
untouched. The compatibility policy is "absent body means no timestamp", which
needs a test rather than a version negotiation.

**2. The derive-and-ratchet precedent already exists in this codebase.**
`txn.Options.ResumeTxnSeq` (`store/txn/txn.go:376`) is derived at recovery and not
persisted, and its doc already carries the "why it is DERIVED and not persisted"
rationale. #2309 is the same shape applied to the commit timestamp, so it follows an
established local convention rather than introducing one. This also matches the
prior art the task cites: InnoDB keeps `TRX_SYS_TRX_ID_STORE` only for upgrades and
instead folds a max over the rollback segments then adds exactly +1; Memgraph derives
`max(delta_ts) + 1` from the WAL and `start_timestamp + 1` from a snapshot; PostgreSQL
persists `nextXid` but still ratchets per record during replay. Two of the three
deliberately removed their persisted counter. **No new persisted counter (AC 2).**

**3. `store/txn` has no MVCC dependency, and — the harder part — the timestamp does
not exist yet when the `OpCommit` frame is written.** `store/txn` never sees a commit
timestamp, so the value has to be threaded in from the layer that allocates it. That
much only makes the task wide: it crosses `graph/mvcc` → `graph/lpg` → `cypher` →
`store/txn` → `store/recovery`.

The ordering is the real obstacle, and an earlier draft of this document got it wrong.
It claimed the timestamp "is known at the point the `OpCommit` frame is written". It is
not. `cypher.ExplicitTx.Commit` calls `Tx.CommitWALOnly` inside its `applyFn` block,
while the commit record is allocated and published by `EndVersionedTx`, called from
the **deferred** `release()` — which therefore runs strictly AFTER the WAL append and
fsync. At encode time there is no timestamp to encode.

So C3a cannot be pure plumbing. It has to move the allocation earlier:

    allocate (NextCommitTS) → encode OpCommit carrying it → fsync → publish (PublishCommitTS)

That is exactly PostgreSQL's shape — the XID is assigned before `XLogFlush`, the
flushed record carries it, and only then is the commit marked visible — and it is
compatible with `mvcc.Clock`'s documented "allocate, store, publish" order, because an
allocated-but-unpublished timestamp is already a state the clock models (it is what
`InFlightCommits` counts). Splitting `EndVersionedTx` into an allocate step and a
publish step is the change C3a actually requires.

**This is a change to commit sequencing, not a refactor.** It moves an allocation
across the fsync boundary in the path the sprint's isolation guarantee rests on, and
it lengthens the window in which a timestamp is allocated but not visible — which
directly affects the contiguous commit frontier, since one in-flight commit holds the
frontier back for every reader. It must therefore be implemented deliberately, with
the frontier cost measured (`MVCCStats.InFlightCommits` is the observable) and with
the crash case in C3c covering a kill between the fsync and the publish. It should
not be folded into an unrelated change.

## The rule the implementation must honour

Derive at recovery as `max(commit timestamp seen across the replayed WAL and the
loaded snapshot) + 1`, and **ratchet rather than trust**: never lower the clock, only
raise it. Two directions must both hold, and both are crash-injection questions
rather than reasoning questions:

- a timestamp that was made **visible must never be re-minted**;
- a timestamp **allocated but never published** must not make a phantom transaction
  visible after recovery.

The second is the subtle one. The WAL is written before the commit timestamp is
published (`fsync → release → publish`), so a crash between the fsync and the publish
leaves a durable `OpCommit` carrying a timestamp that was never visible. Recovery must
treat that as *durable* — the transaction is committed, since the fsync returned — and
the derived floor must therefore sit above it. That is consistent with the existing
`acked-implies-durable` contract and is why the floor is `max + 1` and not `max`.

## Subdivision

The work does not fit one focused pass; each part below is independently complete and
testable.

**C3a — allocate before the fsync, and carry the timestamp into the WAL.** Split
`EndVersionedTx` so the commit timestamp is allocated before the WAL append and
published after the fsync, thread it to `Tx.CommitWALOnly`, and encode it in the
`OpCommit` body (both encoders: `encodeCommitV3Into` on the pooled hot path and
`encodeCommitV3`). Test: a WAL written by the new code decodes to the expected
timestamp; an `OpCommit` with an empty body still replays (the older-file case); and
the contiguous frontier does not regress -- see the sequencing note above.

**C3b — derive and ratchet at recovery.** Track the maximum commit timestamp during
replay, expose it from recovery alongside the existing `ResumeTxnSeq`, add a ratchet
primitive to `mvcc.Clock` that raises both the allocation counter and the visible
frontier and never lowers either, and wire it at open. Test AC 1: after close/reopen,
the next commit timestamp exceeds every previously published one.

**C3c — validate by crash injection (AC 3).** Extend the deterministic battery in
`internal/crashinject` with kill -9 between the fsync and the visibility publish,
asserting on the recovered **graph shape** and not merely on a clean exit — the gap
recorded earlier in this project is that `internal/crashinject` has no graph-shape
assertions.

**C3d — the snapshot's instant (AC 4).** The snapshot/checkpoint header records the
instant it captured, with a test asserting that instant is consistent with the data in
it. This is the piece #2310 (MVCC C4) depends on, and it is the natural boundary
between the two tasks.

AC 6 (documentation of any new field) is discharged per part, in the WAL and snapshot
format docs, as each field lands.
