package sim

import (
	"errors"
	"fmt"
	"io/fs"
)

// This file carries the shared machinery a scenario uses to prove that what its
// checkers read back came out of the SNAPSHOT codec rather than out of a WAL
// replay (rmp #2468).
//
// # Why a forced crossing is needed
//
// A scenario that merely enables [CheckpointConfig] and crashes on a schedule
// proves nothing about the snapshot codec. Which crash lands after which
// checkpoint — and how many WAL bytes each one leaves behind — is a property of
// the seed, so a run can pass with every recovery having replayed the WAL and
// the snapshot components never having been read at all. Worse, a
// [CheckpointConfig] is INERT unless the run loop calls
// [Simulator.maybeCheckpoint]: only [Simulator.Run] does that automatically, so
// a custom loop that forgets the call configures checkpointing and publishes
// nothing (rmp #2457, and again #2464).
//
// [Simulator.crossSnapshotBoundary] removes both accidents. It folds EVERY
// committed op into one snapshot, MEASURES the WAL going to zero, and only then
// crashes — so a checker that runs after it is reading a graph the snapshot
// codec alone reconstructed. [checkSnapshotSourcedRecovery] adjudicates that
// claim on the measurements instead of trusting it.

// snapshotBoundary is the measured evidence of one forced crossing of the
// snapshot boundary: the durable WAL image before and after the checkpoint, and
// how many WAL ops the recovery that followed the crash replayed.
//
// It is the difference between proving recovery used the SNAPSHOT and merely
// observing that the graph survived somehow. The numbers are kept (rather than
// only adjudicated) so a test can log what the run actually measured.
type snapshotBoundary struct {
	// label names the crossing in a violation message, so a report identifies
	// which scenario's boundary failed.
	label string
	// walBefore / walAfter are the byte lengths of the durable WAL image
	// (<dir>/wal on the SimDisk) immediately before and immediately after the
	// forced checkpoint.
	walBefore int64
	walAfter  int64
	// walOpsReplayed is what the post-crash reopen replayed out of the WAL. Zero
	// is the assertion that matters: with an emptied WAL, every node, edge,
	// label and property the recovered engine then serves can only have come out
	// of the snapshot components.
	walOpsReplayed int
	// snapshotPublished records whether a snapshot manifest is durable at
	// <dir>/snapshot/manifest.json after the checkpoint.
	snapshotPublished bool
	// crossed distinguishes a boundary that was really crossed from the zero
	// value, so the adjudicator cannot be satisfied by a run that never called
	// [Simulator.crossSnapshotBoundary] at all.
	crossed bool
}

// reclaimed is how many WAL bytes the checkpoint's prefix truncation gave back.
func (b snapshotBoundary) reclaimed() int64 { return b.walBefore - b.walAfter }

// summary renders the measured numbers for a test log or a failure message.
func (b snapshotBoundary) summary() string {
	return fmt.Sprintf("WAL %d -> %d bytes (reclaimed %d), replayed %d WAL ops, snapshot published=%t",
		b.walBefore, b.walAfter, b.reclaimed(), b.walOpsReplayed, b.snapshotPublished)
}

// simWALSize returns the byte length of the durable WAL image inside disk for a
// store opened with dir. An absent WAL is 0 bytes, not an error: a store whose
// checkpoint reclaimed everything may legitimately hold none.
func simWALSize(disk *SimDisk, dir string) (int64, error) {
	b, err := disk.ReadFile(walPathFor(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return int64(len(b)), nil
}

// crossSnapshotBoundary forces the run across the snapshot boundary and records
// the measurements in [Simulator.boundary]:
//
//  1. measure the durable WAL image;
//  2. publish ONE real checkpoint — a self-sufficient snapshot of every
//     committed op, followed by the prefix truncation that reclaims the WAL the
//     snapshot folded;
//  3. measure the WAL again (an emptied WAL is what makes the next step's claim
//     falsifiable);
//  4. crash SIGKILL-style and reopen through real recovery, recording how many
//     WAL ops the replay contributed.
//
// It reopens with s.store.Config() — the layout the crashed store really used —
// never the WAL-only default: pointing recovery at the wrong (empty) root-level
// WAL key would hollow the scenario out into a check against an empty graph,
// exactly the defect rmp #2464 found in index-diversity.
//
// The crossing counts towards the run's crash and checkpoint statistics, since
// it performs a genuine checkpoint and a genuine crash. Callers that gate on
// in-loop checkpointing having fired ([Simulator.checkCheckpointsFired]) must
// therefore run that gate BEFORE crossing, or the forced checkpoint silences it.
//
// It returns an error — never a violation report — when the simulator has no
// full-stack durable store to checkpoint: that is a scenario misconfiguration
// the run must surface loudly rather than adjudicate.
func (s *Simulator) crossSnapshotBoundary(label string) error {
	if s.store == nil {
		return fmt.Errorf("sim: %s: no durable store to checkpoint"+
			" (the scenario enabled neither crashes, a disk budget, nor checkpointing)", label)
	}
	store, b, err := crossSnapshotBoundaryOn(s.disk, s.store, label)
	if err != nil {
		return err
	}
	s.store = store
	s.engine = NewEngineAdapter(store.Engine())
	s.checkpointCount++
	s.crashCount++
	s.replayedOps += b.walOpsReplayed
	s.boundary = b
	return nil
}

// crossSnapshotBoundaryOn is the store-level body of
// [Simulator.crossSnapshotBoundary]: it performs the checkpoint, the crash and
// the recovery reopen over st, and returns the REPLACEMENT store together with
// the measurements. st must not be used afterwards — it has been crashed.
//
// It exists so a scenario that drives a [SimStore] directly, without a
// [Simulator] around it (the production profile, rmp #2469), gets the identical
// forced crossing rather than an approximation of it. The Simulator method keeps
// the run-statistics bookkeeping, which is the only part that is not about the
// store.
func crossSnapshotBoundaryOn(disk *SimDisk, st *SimStore, label string) (*SimStore, snapshotBoundary, error) {
	var b snapshotBoundary
	cfg := st.Config()
	if cfg.dir == "" {
		return nil, b, fmt.Errorf("sim: %s: the store is WAL-only (no checkpoint dir),"+
			" so no snapshot can be published and no snapshot-sourced recovery can be proven", label)
	}

	b = snapshotBoundary{label: label, crossed: true}
	var err error
	if b.walBefore, err = simWALSize(disk, cfg.dir); err != nil {
		return nil, b, fmt.Errorf("sim: %s: WAL size before checkpoint: %w", label, err)
	}
	if err = st.Checkpoint(); err != nil {
		return nil, b, fmt.Errorf("sim: %s: checkpoint: %w", label, err)
	}
	if b.walAfter, err = simWALSize(disk, cfg.dir); err != nil {
		return nil, b, fmt.Errorf("sim: %s: WAL size after checkpoint: %w", label, err)
	}
	b.snapshotPublished = disk.Exists(cfg.dir + "/" + simSnapshotName + "/manifest.json")

	// SIGKILL-equivalent, byte for byte what [Simulator.maybeCrash] does: drop
	// the live engine and store without a graceful close, revoke every dirent
	// whose parent directory was never fsynced, keep the SimDisk image.
	disk.Crash()
	store, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, b, fmt.Errorf("sim: %s: recovery reopen: %w", label, err)
	}
	b.walOpsReplayed = store.WALOps()
	return store, b, nil
}

// checkSnapshotSourcedRecovery adjudicates that a forced crossing really put the
// snapshot codec — and nothing else — between the writes and the read-back.
//
// Every clause is a way the crossing could have proven nothing while still
// completing:
//
//   - no crossing at all (the zero value): the caller never invoked
//     [Simulator.crossSnapshotBoundary];
//   - no published manifest: the checkpoint produced no durable image;
//   - an empty WAL before the checkpoint: there was no prefix to reclaim, so the
//     truncation below is vacuous and the recovery could have read an already
//     snapshot-only image from an earlier cycle;
//   - a non-empty WAL after it: the checkpoint refused to truncate (which is
//     what the checkpointer's self-sufficiency re-verification does when a
//     component is missing), so recovery still had WAL frames to fall back on;
//   - a non-zero replay: whatever the checkers then read may have come from
//     those frames rather than from the snapshot.
func checkSnapshotSourcedRecovery(tick int64, b snapshotBoundary) []Violation {
	const op = "snapshot-sourced recovery"
	fail := func(format string, args ...any) []Violation {
		return []Violation{{
			Kind: ViolationACIDDurability, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", b.label) + fmt.Sprintf(format, args...),
		}}
	}
	if !b.crossed {
		return fail("the run never crossed the snapshot boundary: every read-back below could have been" +
			" served by a WAL replay, so the snapshot codec was never exercised")
	}
	if !b.snapshotPublished {
		return fail("no snapshot manifest was published: the checkpoint produced no durable image")
	}
	if b.walBefore <= 0 {
		return fail("the WAL was empty before the checkpoint: there was no prefix to reclaim," +
			" so the truncation proves nothing about where the recovered state came from")
	}
	if b.walAfter != 0 {
		return fail("the checkpoint left %d of %d WAL bytes on disk (reclaimed %d): recovery could still"+
			" replay them, so a surviving value is not evidence the snapshot codec round-tripped it",
			b.walAfter, b.walBefore, b.reclaimed())
	}
	if b.walOpsReplayed != 0 {
		return fail("recovery replayed %d WAL ops although the checkpoint emptied the WAL (%d -> 0 bytes):"+
			" the recovered graph did not come from the snapshot alone",
			b.walOpsReplayed, b.walBefore)
	}
	return nil
}
