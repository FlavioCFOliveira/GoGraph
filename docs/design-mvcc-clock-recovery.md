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

**C3a — allocate before the fsync, and carry the timestamp into the WAL. DONE.**

Implemented as specified. `Graph.AllocateCommitTS` reserves the instant without
publishing it; `Graph.endWrite` publishes that reservation instead of minting its
own. Both encoders carry it, `Tx.CommitWALOnly` takes it, and `recovery.Op.CommitTS`
reads it back — reporting **presence separately from value**, so an absent timestamp
contributes nothing to the derived maximum while a genuine zero contributes zero.

**The obligation the sequencing change creates, and where it is discharged.** An
allocated timestamp that is neither published nor abandoned stalls the contiguous
frontier *permanently*. The discharge therefore lives in `endWrite`, which every path
reaches — publish on success, `Clock.AbandonCommitTS` on abort and on the
versioned-nothing exit — rather than beside each caller, because a discharge a new
caller can forget fails silently rather than loudly.

Tested at three levels, and **every instrument was validated against a build with the
defect injected**: the wire layout and the two encoders' byte-identity
(`store/txn`); presence-vs-zero-vs-absent on decode (`store/recovery`); the
allocation's idempotence, its identity with what is actually published, and the
recycled-state hazard (`graph/lpg`); and end-to-end, that a real engine commit's
instant reaches the file and that `InFlightCommits` returns to zero on the
versioned-nothing path (`cypher`).

**One documented claim was corrected by measurement.** The reset in
`acquireWriteCtx` was described as the load-bearing clear; removing it changed
nothing, because every `endWrite` discharge path already zeroes the field. It is
defence in depth, and the comment now says so.

**C3b — derive and ratchet at recovery. DONE.**

`recovery.Result.MaxCommitTS` tracks the maximum during replay, `mvcc.Clock.RatchetTo`
raises both the allocation counter and the visible frontier and never lowers either,
and `lpg.Graph.RestoreMVCCClock` applies the `MaxCommitTS+1` floor.

**Wired inside recovery, not left to the embedder — and the precedent is why.**
`txn.Options.ResumeTxnSeq` is the same shape one layer down, and **no production
caller sets it**: it appears only in its own tests. A restoration every reopen path
must remember is one some reopen path will forget, and the failure is silent —
writes keep succeeding at instants that collide with durable ones.

### The obvious test passes without the fix, and that was measured

The natural acceptance test — commit six transactions, reopen, check the clock — is
**not discriminating**. Injecting the defect (removing the restore entirely) left it
green.

**Recovery mints an instant per replayed OP, not per transaction.** Six three-op
`CREATE` statements produce `WALOps=18` and drive the recovered clock to **18**,
while the durable maximum is **6**. Replay overshoots on its own, so the restore is a
no-op in that shape and the test proved nothing.

The restore only bites when the durable maximum **exceeds** what replay reaches —
which is exactly the production case a snapshot creates, since the snapshot folds the
WAL prefix and leaves few ops to replay against high timestamps. The test now appends
a commit marker carrying `1_000_000` to reproduce that relationship without a
snapshot's machinery, and against the injected defect it fails with `clock reads 18,
at or below the 1000000 already durable`.

**Noted, not chased:** that replay stamps each op with its own instant rather than
one per transaction is a separate observation. It costs nothing during recovery,
where there are no readers to observe a partially-applied transaction, but it does
mean the recovered clock is inflated relative to the record. It becomes relevant to
C3d/C4, where a snapshot must name the instant it captured.

**C3c — validate by crash injection (AC 3). DONE.**

A `mvcc.commit.post-fsync-pre-publish` breakpoint sits in `commitUnderBarrier`
between the WAL fsync and the bracket unwind that publishes the instant, a helper
scenario commits through the Cypher engine and crashes there, and
`TestCrashRecovery_MVCCClock_PostFsyncPrePublish` asserts the recovered **graph
shape** and the recovered **clock**. `GOGRAPH_CRASH_AFTER` skips the seed
transactions so the crash lands on the last one, leaving a published prefix to
distinguish from the durable-but-unpublished commit.

**The recorded gap was stale.** This spec said `internal/crashinject` has no
graph-shape assertions; task #2270 had already added them
(`internal/crashinject/graph_shape_test.go`), so the new test reuses that
harness's `runAndAssertKilled` rather than building one.

**The clock assertion is a GUARD, not a discriminator, and that was measured.**
Removing the clock restore from recovery entirely leaves the crash test green.
WAL-only replay mints an instant per **op** while the durable maximum counts
**transactions**, and ops-per-transaction is always at least one — so the replayed
clock necessarily overshoots and the restore is unobservable in any WAL-only
scenario.

**Consequence for C3d, and it is the useful one:** the restore becomes load-bearing
precisely when a snapshot folds the WAL prefix, leaving few ops to replay against
high durable instants. That is not a reason to defer it — a snapshot-bearing
directory is the normal production shape — but it does mean the snapshot must carry
its captured instant for the derivation to be complete, which is exactly what C3d
is for. The discriminating coverage until then is
`TestClockRestore_NextCommitExceedsEveryPreviouslyPublishedInstant`, which
manufactures the relationship with a hand-appended marker.

**C3d — the snapshot's instant (AC 4). DONE.**

`snapshot.Capture` records the instant at capture time — read **before** any
component is serialised, so the recorded instant can only be at or *before* the state
the image contains, never after it — `Manifest.CommitTS` carries it to disk, and
recovery seeds the derived floor from it before folding the WAL's maximum on top.

**No manifest version bump.** The manifest is JSON: an older reader ignores an
unknown field, a newer reader on an older manifest decodes zero, and `omitempty`
keeps a timestamp-less manifest byte-identical to what previous builds wrote. Same
"absent means no timestamp" policy as the `OpCommit` body.

**This is the half of the derivation that actually bites.** C3c measured that the WAL
half is unobservable — replay overshoots on its own. A checkpoint truncates the WAL
prefix, so the instants of everything the image folded are no longer in the log at
all; deriving from the WAL alone would restore a clock far below data the image holds
and then re-mint instants that are durably in it. Removing the seed fails
`TestSnapshotInstant_RecoveryDerivesTheFloorFromTheImage` with `derived a maximum of
0 from ... an image recorded at 4`.

**A second defect was found by validating the first fix.** Recovery reads the
snapshot's instant and then the WAL's, and the WAL step was a plain assignment. That
discards the image's instant whenever the surviving suffix carries a smaller one —
and after a checkpoint with no subsequent writes the suffix carries **nothing**, so
the floor collapses to zero. That is the ordinary state of a freshly checkpointed
directory. It is now a maximum, and
`TestSnapshotInstant_AnEmptyWALDoesNotEraseTheImagesInstant` covers it; the
WAL-removed sibling could not, because it never reaches the WAL branch at all.

**What C4 (#2310) still owes.** The capture is taken under the caller's exclusion, so
"the instant it captured" is currently exact by construction. Once writers continue
during a capture that stops being true, and the recorded instant becomes the
definition of what the image contains rather than a description of it.

AC 6 (documentation of any new field) is discharged per part, in the WAL and snapshot
format docs, as each field lands.
