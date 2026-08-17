package sim

// mvcc_clock_recovery.go — the machinery a scenario uses to prove that the MVCC
// clock and the transaction sequence are RECONCILED against the durable record
// when a reopen crosses a snapshot boundary (rmp #2469).
//
// # The case that was never entered
//
// The MVCC crash scenarios never checkpoint, so every recovery re-derives the
// clock from a COMPLETE WAL: the prefix carrying the early timestamps is always
// there to be read. The case that matters is the other one — the checkpoint has
// truncated that prefix away, so the only surviving record of those instants is
// the one the snapshot manifest carries ([snapshot.Manifest.CommitTS], folded
// into the derived floor by recovery, rmp #2309/#2520). A run that never crosses
// a snapshot boundary cannot tell a floor that was reconciled from one that was
// merely re-derived from a WAL that happened to still hold everything.
//
// # Why the WAL is the instrument
//
// Every durable v3 commit marker carries the transaction's sequence AND the MVCC
// instant it became visible at ([recovery.Op.CommitTS], written before the fsync
// so recovery can derive the clock from the log rather than from a persisted
// counter). Reading the markers out of the durable image is therefore a DIRECT
// measurement of both quantities, from the same bytes recovery reads, rather
// than an inference from the engine's in-memory counters. The clock floor is
// read from the engine ([SimStore.ClockNow]) precisely because the WAL cannot
// speak for what recovery did with what it read.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// tmplCreateMVCCBeacon creates one post-recovery beacon node. It carries a label
// of its own so the beacons stay invisible to the Person-keyed durability
// ledgers every durable scenario adjudicates: the beacons exist to make a commit
// marker, not to be part of anyone's expected set.
const tmplCreateMVCCBeacon = "CREATE (n:MVCCBeacon {name:$name})"

// mvccPostRecoveryBeacons is how many transactions a scenario commits after a
// reopen to observe the instants and sequences the recovered store mints. Four
// is enough for the strict-monotonicity clause to have something to say while
// costing four single-node commits.
const mvccPostRecoveryBeacons = 4

// walCommitMarker is one durable transaction-commit marker read back out of a
// WAL image: where it sits, which transaction sequence it closes, and the MVCC
// instant that transaction became visible at.
type walCommitMarker struct {
	// off is the marker frame's byte offset in the WAL image. It is what
	// partitions the markers a recovery READ from the ones committed after it,
	// since the WAL is append-only between truncations.
	off int64
	seq uint64
	ts  uint64
	// hasTS distinguishes "no timestamp" from "the timestamp zero". A marker
	// without one is the pre-rmp #2309 on-disk shape and contributes nothing to
	// the derived floor, so it is a finding rather than a small number.
	hasTS bool
}

// simWALCommitMarkers returns every transaction-commit marker carried by the
// durable WAL image of a store opened with dir, in file order.
//
// A torn tail is not an error: it is the ordinary state of a WAL a crash
// interrupted, and the markers before it are exactly the ones recovery applied.
// An absent WAL yields no markers, which is what a checkpoint that reclaimed
// everything leaves behind.
func simWALCommitMarkers(disk *SimDisk, dir string) ([]walCommitMarker, error) {
	image, err := disk.ReadFile(walPathFor(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var (
		out []walCommitMarker
		off int64
	)
	for f := range wal.NewReader(bytes.NewReader(image), nil).Frames() {
		op, derr := recovery.Decode(f.Payload)
		if derr == nil && op.Kind == txn.OpCommit {
			ts, hasTS := op.CommitTS()
			out = append(out, walCommitMarker{off: off, seq: op.TxnSeq, ts: ts, hasTS: hasTS})
		}
		off += int64(wal.HeaderSize + len(f.Payload))
	}
	return out, nil
}

// simSnapshotInstant reports the MVCC instant the published snapshot under dir
// was captured at ([snapshot.Manifest.CommitTS]) and whether a manifest was
// there to read at all. A manifest that fails its integrity trailer is an error,
// never a silent zero: zeroing this field is exactly the silent drop rmp #2467
// measured and rmp #2520 closed.
func simSnapshotInstant(disk *SimDisk, dir string) (uint64, bool, error) {
	path := dir + "/" + simSnapshotName + "/manifest.json"
	if !disk.Exists(path) {
		return 0, false, nil
	}
	man, err := snapshot.ReadManifestFileFS(simSnapshotFS{disk: disk}, path)
	if err != nil {
		return 0, false, fmt.Errorf("sim: read snapshot manifest %s: %w", path, err)
	}
	return man.CommitTS, true, nil
}

// mvccRecoveryEvidence is the measured record of ONE reopen: what the durable
// image the recovery read carried, what the reopened engine derived from it, and
// what the transactions committed after it wrote back.
//
// It is kept as measurements rather than as a verdict so a test can log what the
// run actually observed, and so [checkMVCCRecovery] adjudicates numbers instead
// of trusting a claim.
type mvccRecoveryEvidence struct {
	// label names the reopen in a violation message.
	label string
	// snapshotInstant / hasSnapshot are the manifest's recorded capture instant
	// and whether a snapshot was published at all.
	snapshotInstant uint64
	hasSnapshot     bool
	// imageMaxTS and imageMaxSeq are the maxima over the commit markers the
	// durable WAL image held AT THE REOPEN — every instant and sequence that was
	// already spent when the recovered store started minting.
	imageMaxTS  uint64
	imageMaxSeq uint64
	// imageSeqs is the set of sequences still PRESENT in that image. A
	// post-recovery transaction that re-mints one of them puts two different
	// transactions under one number in one WAL (rmp #2302).
	imageSeqs map[uint64]struct{}
	// imageMarkers and walLenAtReopen size the image the recovery read.
	imageMarkers   int
	walLenAtReopen int64
	// walOpsReplayed is what the reopen replayed out of the WAL. Zero, with an
	// empty WAL and a published snapshot, is the PURE-SNAPSHOT recovery: the
	// clock floor can then only have come from the manifest.
	walOpsReplayed int
	// resumedTxnSeq and recoveredMaxTS are what the store says it recovered:
	// the sequence it continued from and the durable instant it folded.
	resumedTxnSeq  uint64
	recoveredMaxTS uint64
	// clockFloor is the MVCC clock's published instant immediately after the
	// reopen, before any post-recovery commit.
	clockFloor uint64
	// post holds the commit markers the post-recovery transactions made durable,
	// in commit order.
	post []walCommitMarker
	// measured and observed record that the two halves actually ran, so the
	// adjudicator cannot be satisfied by a zero value.
	measured bool
	observed bool
}

// durableMaxTS is the highest MVCC instant the recovered image carried, over
// BOTH the surviving WAL and the snapshot manifest. It is the floor every
// post-recovery commit must exceed: an instant that reached the durable record
// is spent, whether or not any reader ever saw it.
func (e *mvccRecoveryEvidence) durableMaxTS() uint64 {
	if e.snapshotInstant > e.imageMaxTS {
		return e.snapshotInstant
	}
	return e.imageMaxTS
}

// pureSnapshot reports whether this reopen was sourced by the snapshot ALONE:
// a published manifest, an empty WAL, and nothing replayed out of it.
func (e *mvccRecoveryEvidence) pureSnapshot() bool {
	return e.hasSnapshot && e.walOpsReplayed == 0 && e.imageMarkers == 0 && e.walLenAtReopen == 0
}

// snapshotPlusWALTail reports whether this reopen read a published snapshot AND
// replayed a surviving WAL tail on top of it.
func (e *mvccRecoveryEvidence) snapshotPlusWALTail() bool {
	return e.hasSnapshot && e.walOpsReplayed > 0
}

// summary renders the measured numbers for a test log or a failure message.
func (e *mvccRecoveryEvidence) summary() string {
	kind := "WAL-only"
	switch {
	case e.pureSnapshot():
		kind = "pure snapshot"
	case e.snapshotPlusWALTail():
		kind = "snapshot+WAL tail"
	}
	return fmt.Sprintf("%s: %s recovery, image=%d markers/%d bytes (maxTS=%d maxSeq=%d), snapshot instant=%d (present=%t), "+
		"replayed %d WAL ops, resumed seq=%d, recovered maxTS=%d, clock floor=%d, %d post-recovery commits",
		e.label, kind, e.imageMarkers, e.walLenAtReopen, e.imageMaxTS, e.imageMaxSeq, e.snapshotInstant,
		e.hasSnapshot, e.walOpsReplayed, e.resumedTxnSeq, e.recoveredMaxTS, e.clockFloor, len(e.post))
}

// measureMVCCRecovery reads what a just-reopened store recovered: the durable
// image it read (WAL markers and the snapshot's recorded instant), and the clock
// floor and sequence it came up on.
//
// It must be called BEFORE any post-recovery commit, since both the image and
// the floor are the state as recovery left it.
func measureMVCCRecovery(disk *SimDisk, st *SimStore, label string) (mvccRecoveryEvidence, error) {
	ev := mvccRecoveryEvidence{label: label, imageSeqs: make(map[uint64]struct{})}
	dir := st.Config().dir

	markers, err := simWALCommitMarkers(disk, dir)
	if err != nil {
		return ev, fmt.Errorf("sim: %s: read WAL commit markers: %w", label, err)
	}
	for _, m := range markers {
		if m.hasTS && m.ts > ev.imageMaxTS {
			ev.imageMaxTS = m.ts
		}
		if m.seq > ev.imageMaxSeq {
			ev.imageMaxSeq = m.seq
		}
		ev.imageSeqs[m.seq] = struct{}{}
	}
	ev.imageMarkers = len(markers)

	if ev.walLenAtReopen, err = simWALSize(disk, dir); err != nil {
		return ev, fmt.Errorf("sim: %s: WAL size at reopen: %w", label, err)
	}
	if dir != "" {
		if ev.snapshotInstant, ev.hasSnapshot, err = simSnapshotInstant(disk, dir); err != nil {
			return ev, err
		}
	}
	ev.walOpsReplayed = st.WALOps()
	ev.resumedTxnSeq = st.ResumedTxnSeq()
	ev.recoveredMaxTS = st.RecoveredMaxCommitTS()
	ev.clockFloor = st.ClockNow()
	ev.measured = true
	return ev, nil
}

// observePostRecoveryCommits commits n single-node transactions through the
// REOPENED store and records the commit markers they made durable.
//
// The markers are identified by byte offset: the WAL is append-only between
// truncations and nothing truncates it here, so every marker at or past the
// length measured at the reopen was written by one of these transactions. That
// keeps the partition independent of the timestamps and sequences under test —
// identifying a post-recovery commit BY its timestamp would assume the very
// property the oracle exists to falsify.
func (e *mvccRecoveryEvidence) observePostRecoveryCommits(ctx context.Context, disk *SimDisk, st *SimStore, n int) error {
	if !e.measured {
		return fmt.Errorf("sim: %s: observePostRecoveryCommits before measureMVCCRecovery", e.label)
	}
	adapter := NewEngineAdapter(st.Engine())
	for i := range n {
		name := fmt.Sprintf("mvcc-beacon-%s-%d", e.label, i)
		res, err := adapter.RunWrite(ctx, tmplCreateMVCCBeacon, map[string]any{"name": name})
		if err != nil {
			return fmt.Errorf("sim: %s: post-recovery commit %d: %w", e.label, i, err)
		}
		if err := res.Close(); err != nil {
			return fmt.Errorf("sim: %s: post-recovery commit %d close: %w", e.label, i, err)
		}
	}
	markers, err := simWALCommitMarkers(disk, st.Config().dir)
	if err != nil {
		return fmt.Errorf("sim: %s: read post-recovery WAL commit markers: %w", e.label, err)
	}
	for _, m := range markers {
		if m.off >= e.walLenAtReopen {
			e.post = append(e.post, m)
		}
	}
	e.observed = true
	return nil
}

// checkMVCCRecovery adjudicates that a reopen reconciled the MVCC clock and the
// transaction sequence against the durable record.
//
// Every clause is a way a reopen could look healthy and not be:
//
//   - never measured / never observed: the caller skipped a half, so the numbers
//     below are the zero value rather than a measurement;
//   - no post-recovery commit was observed: nothing was minted after the reopen,
//     so no clause below can fail and the whole check is vacuous;
//   - a commit marker carrying NO instant: the durable record no longer says when
//     the transaction became visible, so the next recovery's floor cannot be
//     derived from it;
//   - an instant at or below the durable maximum: a reader could reach a version
//     that is simultaneously in its past and its future, and an instant the image
//     already contains would be minted a second time;
//   - an instant repeated within the post-recovery run: two transactions made
//     visible at one instant;
//   - a sequence still present in the recovered image: one WAL image holding two
//     different transactions under one number — the ambiguity that fuses an
//     orphaned prefix into the wrong transaction (rmp #2302);
//   - a sequence at or below the image maximum: the sequence did not resume, it
//     restarted;
//   - a pure-snapshot recovery whose clock floor is below the instant the
//     manifest recorded: the reconciliation against the image never happened —
//     the case that only exists once the WAL prefix has been truncated away;
//   - a pure-snapshot recovery that DERIVED a maximum below that instant: the
//     floor clause above can be satisfied by accident, because rehydrating an
//     image mints instants of its own (measured: about three per restored node,
//     so a wide graph lifts the clock past its own recorded instant whether or
//     not that instant was ever read). This clause is the one that cannot be
//     satisfied incidentally: with an empty WAL, the derived maximum can only
//     have come from the manifest.
func checkMVCCRecovery(tick int64, e *mvccRecoveryEvidence) []Violation {
	const op = "MVCC clock and sequence recovery"
	var out []Violation
	fail := func(kind ViolationKind, format string, args ...any) {
		out = append(out, Violation{
			Kind: kind, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", e.label) + fmt.Sprintf(format, args...),
		})
	}

	if !e.measured {
		fail(ViolationACIDDurability, "the reopen was never measured: no image, floor or sequence was read,"+
			" so every clause below would pass on the zero value")
		return out
	}
	if !e.observed {
		fail(ViolationACIDDurability, "no post-recovery commit was ever driven: the clock and the sequence the"+
			" recovered store mints were never observed")
		return out
	}
	if len(e.post) == 0 {
		fail(ViolationACIDDurability, "the post-recovery transactions left no durable commit marker at or past"+
			" the WAL length measured at the reopen (%d bytes): there is nothing to adjudicate", e.walLenAtReopen)
		return out
	}

	durableMax := e.durableMaxTS()
	seen := make(map[uint64]struct{}, len(e.post))
	for i, m := range e.post {
		if !m.hasTS {
			fail(ViolationACIDDurability, "post-recovery commit %d (seq %d) carries NO MVCC instant:"+
				" the next recovery cannot derive a clock floor from it", i, m.seq)
			continue
		}
		if m.ts <= durableMax {
			fail(ViolationACIDIsolation, "post-recovery commit %d was made visible at instant %d, which does not exceed"+
				" the durable maximum %d (WAL image max %d, snapshot instant %d): an instant the recovered image"+
				" already contains has been minted again",
				i, m.ts, durableMax, e.imageMaxTS, e.snapshotInstant)
		}
		if _, dup := seen[m.ts]; dup {
			fail(ViolationACIDIsolation, "post-recovery commits share instant %d: two transactions became visible"+
				" at one instant", m.ts)
		}
		seen[m.ts] = struct{}{}

		if _, reused := e.imageSeqs[m.seq]; reused {
			fail(ViolationACIDAtomicity, "post-recovery commit %d re-minted transaction sequence %d, which the"+
				" recovered WAL image still carries: one WAL now holds two different transactions under one"+
				" sequence number (resumed from %d)", i, m.seq, e.resumedTxnSeq)
		} else if m.seq <= e.imageMaxSeq {
			fail(ViolationACIDAtomicity, "post-recovery commit %d used transaction sequence %d, at or below the"+
				" image maximum %d: the sequence restarted instead of resuming (resumed from %d)",
				i, m.seq, e.imageMaxSeq, e.resumedTxnSeq)
		}
	}

	if e.pureSnapshot() {
		if e.clockFloor < e.snapshotInstant {
			fail(ViolationACIDDurability, "the clock floor after a PURE-SNAPSHOT recovery is %d, below the instant %d the"+
				" manifest recorded: the WAL prefix carrying those timestamps was truncated away, so the floor could"+
				" only have been reconciled from the image — and it was not", e.clockFloor, e.snapshotInstant)
		}
		if e.recoveredMaxTS < e.snapshotInstant {
			fail(ViolationACIDDurability, "a PURE-SNAPSHOT recovery derived a durable maximum of %d from an image"+
				" recording instant %d: the recorded instant was not folded into the floor. The floor itself reads %d"+
				" only because rehydrating the image mints instants of its own, which is a property of the image's SIZE"+
				" and not of its instant", e.recoveredMaxTS, e.snapshotInstant, e.clockFloor)
		}
	}
	return out
}
