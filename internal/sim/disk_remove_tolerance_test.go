package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// disk_remove_tolerance_test.go — the engine-level half of rmp #2536.
//
// disk_remove_crash_test.go pins the MODEL: a crash after an unlink but before
// the parent fsync can leave the name in place. This file spends that capability
// on the thing it was built for, and the distinction matters — the audit's F2
// verdict was not "the model is wrong" but "the model BLINDS", and a blindness is
// only closed once the branch it hid is shown running.
//
// Two branches of the real engine exist solely to tolerate an artefact a previous
// crash left behind:
//
//   - store/recovery/recovery.go's RemoveAll(<dir>/snapshot.tmp), the stale
//     staging cleanup, whose own comment says the staging directory "is always
//     safe to drop";
//   - store/snapshot's RemoveAll(<dir>/snapshot.bak), the stale backup cleanup at
//     the head of the publish protocol, which is idempotent because "recovery may
//     already have promoted or discarded it".
//
// Before #2536 the harness could reach those states only by crashing BEFORE the
// removal was issued. It could not reach them the way a deployment does: the
// removal ran, the caller saw it succeed, and the unlink never reached stable
// storage. Both tests below therefore assert on
// [SimDisk.RemoveHitCountForPath] rather than on the artefact's absence, because
// a cleanup that ran as a no-op and a cleanup that really deleted a leftover are
// indistinguishable from outside — and each test carries the control arm in which
// that counter reads zero, which is what makes the demonstration falsifiable.

// publishSnapshot writes a self-sufficient snapshot of g into <dir>/snapshot
// through the real publish protocol and RETURNS its error, so a test can drive
// the protocol's own failure paths. writeSnapshotTo is the fatal-on-error variant
// used where the publish is expected to succeed.
func publishSnapshot(disk *SimDisk, dir string, g *lpg.Graph[string, float64], cs *csr.CSR[float64]) error {
	return snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		simSnapshotFS{disk: disk}, dir+"/snapshot", cs, g, txn.NewStringCodec(), nil,
	)
}

// strandStagingDirectory leaves a REAL leftover <dir>/snapshot.tmp on disk by
// driving the publish protocol's own abort path: the publish rename fails, the
// writer restores the archived snapshot, and the staging tree it had already
// written and fsync'd is left behind untouched.
//
// The restore rename is pinned durable so that the only not-yet-durable metadata
// afterwards is whatever the test itself issues. That is the same arm pairing the
// checkpoint crash storm uses for its publish-rename window.
func strandStagingDirectory(t *testing.T, disk *SimDisk, dir string, order int) {
	t.Helper()
	g, cs := buildSnapshotGraph(t, order)
	disk.ArmRenameFaultForPath(dir + "/snapshot")
	disk.ArmRenameWritebackForPath(dir + "/snapshot")
	if err := publishSnapshot(disk, dir, g, cs); err == nil {
		t.Fatal("the publish rename was expected to fail: no staging tree was stranded")
	}
	if got := disk.RenameFaultCount(); got != 1 {
		t.Fatalf("RenameFaultCount = %d, want 1: the fault arm never matched the publish rename", got)
	}
	if !disk.Exists(dir + "/snapshot.tmp/manifest.json") {
		t.Fatal("the aborted publish left no staging directory behind")
	}
}

// TestRemoveTolerance_RecoveryStaleStagingCleanupIsReachedFromTheRemovalSide
// drives recovery's stale-staging cleanup twice over the SAME leftover: once
// where its unlink fails to reach stable storage before the crash, and once
// afterwards, when the artefact it thought it had deleted is back.
//
// That is the exact state F2 named unreachable — "a stale <dir>/snapshot.tmp
// surviving recovery's own best-effort staging cleanup" — and the counter is what
// distinguishes the branch running from the branch idling.
func TestRemoveTolerance_RecoveryStaleStagingCleanupIsReachedFromTheRemovalSide(t *testing.T) {
	const staging = "db/snapshot.tmp"
	for _, tc := range []struct {
		name string
		// arm selects which branch of the unlink's crash window the first
		// recovery's cleanup takes.
		arm func(d *SimDisk)
		// wantSurvives is whether the leftover is expected back after the crash,
		// and wantSecondHit whether the SECOND recovery's cleanup then finds
		// something to delete.
		wantSurvives  bool
		wantSecondHit int64
	}{
		{
			name:          "unlink does not reach stable storage",
			arm:           func(d *SimDisk) { d.ArmRemoveRollbackForPath(staging) },
			wantSurvives:  true,
			wantSecondHit: 1,
		},
		{
			// The control. The removal sticks, so the second recovery's cleanup
			// runs over an empty path — which is the ONLY thing the harness could
			// produce before #2536, and it exercises nothing.
			name:          "control: unlink reaches stable storage",
			arm:           func(d *SimDisk) { d.ArmRemoveWritebackForPath(staging) },
			wantSurvives:  false,
			wantSecondHit: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk := NewSimDisk(NewSeed(0x2536_1001), 0) // no data faults: isolate the dirent model

			// A live snapshot of 4 nodes, published with the full fsync protocol.
			g1, cs1 := buildSnapshotGraph(t, 4)
			writeSnapshotTo(t, disk, "db", g1, cs1)
			// An aborted publish of a 9-node snapshot leaves a real staging tree.
			strandStagingDirectory(t, disk, "db", 9)

			// First recovery. Its best-effort RemoveAll of the staging tree must
			// find the leftover, and the arm decides whether that unlink sticks.
			tc.arm(disk)
			before := disk.RemoveHitCountForPath(staging)
			if got := recoverFromSimDisk(t, disk, "db"); got != 4 {
				t.Fatalf("first recovery: order=%d, want 4 (the live snapshot)", got)
			}
			if delta := disk.RemoveHitCountForPath(staging) - before; delta != 1 {
				t.Fatalf("RemoveHitCountForPath(%s) delta across the first recovery = %d, want 1: "+
					"the stale-staging cleanup did not delete the leftover, so this test is not driving that branch",
					staging, delta)
			}

			// Crash inside the unlink's window.
			pendingBefore := disk.PendingRemoveCount()
			disk.Crash()
			pending, restored := disk.LastCrashRemoveOutcome()

			if got := disk.Exists(staging + "/manifest.json"); got != tc.wantSurvives {
				t.Fatalf("staging leftover present after the crash = %t, want %t (pending before=%d, crash pending=%d restored=%d)",
					got, tc.wantSurvives, pendingBefore, pending, restored)
			}

			// Second recovery, over the state the crash actually left.
			before = disk.RemoveHitCountForPath(staging)
			if got := recoverFromSimDisk(t, disk, "db"); got != 4 {
				t.Fatalf("second recovery: order=%d, want 4 — a leftover staging directory must not change what is recovered", got)
			}
			if delta := disk.RemoveHitCountForPath(staging) - before; delta != tc.wantSecondHit {
				t.Fatalf("RemoveHitCountForPath(%s) delta across the second recovery = %d, want %d",
					staging, delta, tc.wantSecondHit)
			}
			t.Logf("staging leftover survived=%t; cleanup hits: first=1 second=%d; crash adjudicated pending=%d restored=%d",
				tc.wantSurvives, tc.wantSecondHit, pending, restored)
		})
	}
}

// TestRemoveTolerance_StaleBackupCleanupIsReachedFromTheRemovalSide is the same
// demonstration for the other artefact: the publish protocol's happy-path
// RemoveAll of the archived backup does not reach stable storage, so after the
// crash a stale <dir>/snapshot.bak sits beside a perfectly good live snapshot —
// the second state F2 named unreachable.
//
// Two engine behaviours are then exercised over it: recovery must ignore a stale
// backup while the live snapshot is present (it promotes only when live is
// absent), and the NEXT publish's stale-backup cleanup must delete it. The
// counter delta across that publish is 2 with the leftover and 1 without, since
// the publish always archives and then drops a backup of its own.
func TestRemoveTolerance_StaleBackupCleanupIsReachedFromTheRemovalSide(t *testing.T) {
	const backup = "db/snapshot.bak"
	for _, tc := range []struct {
		name             string
		arm              func(d *SimDisk)
		wantSurvives     bool
		wantPublishHits  int64
		wantPendingAfter int
	}{
		{
			name:             "unlink does not reach stable storage",
			arm:              func(d *SimDisk) { d.ArmRemoveRollbackForPath(backup) },
			wantSurvives:     true,
			wantPublishHits:  2, // the stale leftover, then this publish's own archive
			wantPendingAfter: 1,
		},
		{
			name:             "control: unlink reaches stable storage",
			arm:              func(d *SimDisk) { d.ArmRemoveWritebackForPath(backup) },
			wantSurvives:     false,
			wantPublishHits:  1, // this publish's own archive only
			wantPendingAfter: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk := NewSimDisk(NewSeed(0x2536_2001), 0)

			// Publish #1: the snapshot that will become the archived backup.
			g1, cs1 := buildSnapshotGraph(t, 4)
			writeSnapshotTo(t, disk, "db", g1, cs1)

			// Publish #2, clean: it archives #1 to .bak, publishes, fsyncs the
			// parent, and then drops the backup — the removal this test intercepts.
			g2, cs2 := buildSnapshotGraph(t, 9)
			tc.arm(disk)
			writeSnapshotTo(t, disk, "db", g2, cs2)
			if got := disk.RemoveHitCountForPath(backup); got != 1 {
				t.Fatalf("RemoveHitCountForPath(%s) = %d, want 1: the happy-path backup cleanup found nothing to delete",
					backup, got)
			}
			if got := disk.PendingRemoveCount(); got != tc.wantPendingAfter {
				t.Fatalf("PendingRemoveCount = %d, want %d: the removal's crash window is not what this test assumes",
					got, tc.wantPendingAfter)
			}

			disk.Crash()
			pending, restored := disk.LastCrashRemoveOutcome()
			if got := disk.Exists(backup + "/manifest.json"); got != tc.wantSurvives {
				t.Fatalf("stale backup present after the crash = %t, want %t (crash pending=%d restored=%d)",
					got, tc.wantSurvives, pending, restored)
			}
			// Whatever happened to the backup, the live snapshot was fully
			// published and its parent fsync'd, so it must be intact.
			if !disk.Exists("db/snapshot/manifest.json") {
				t.Fatal("the live snapshot did not survive: the removal model reached beyond the name it was armed on")
			}

			// Recovery must not be confused by a stale backup: with live present
			// it promotes nothing and recovers publish #2.
			if got := recoverFromSimDisk(t, disk, "db"); got != 9 {
				t.Fatalf("recovery with a stale backup present: order=%d, want 9", got)
			}

			// Publish #3: its stale-backup cleanup is the branch under test.
			g3, cs3 := buildSnapshotGraph(t, 6)
			before := disk.RemoveHitCountForPath(backup)
			writeSnapshotTo(t, disk, "db", g3, cs3)
			if delta := disk.RemoveHitCountForPath(backup) - before; delta != tc.wantPublishHits {
				t.Fatalf("RemoveHitCountForPath(%s) delta across the next publish = %d, want %d: "+
					"with a leftover the stale-backup cleanup deletes it AND the happy-path cleanup drops this publish's own archive",
					backup, delta, tc.wantPublishHits)
			}
			if got := recoverFromSimDisk(t, disk, "db"); got != 6 {
				t.Fatalf("recovery after the republish: order=%d, want 6", got)
			}
			t.Logf("stale backup survived=%t; cleanup hits across the next publish=%d; crash adjudicated pending=%d restored=%d",
				tc.wantSurvives, tc.wantPublishHits, pending, restored)
		})
	}
}
