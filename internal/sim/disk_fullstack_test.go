package sim

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// This file proves rmp #1546 non-vacuously: the snapshot + checkpoint
// publish/promote paths can now be crashed mid-flight on the in-memory
// [SimDisk], and the dirent-durability model makes the directory fsyncs
// load-bearing — removing one (the guard test) loses data across a crash.
//
// All adapters route through simSnapshotFS / simRecoveryFS / simCheckpointBackend
// (diskfs.go). The store keys are string with float64 weights, matching the
// rest of the harness.

// buildSnapshotGraph returns a small directed multigraph with n nodes chained
// 0->1->...->(n-1), plus its CSR, ready to hand to the snapshot writers.
func buildSnapshotGraph(t *testing.T, n int) (*lpg.Graph[string, float64], *csr.CSR[float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		if err := g.AddNode(nodeKey(i)); err != nil {
			t.Fatalf("AddNode %d: %v", i, err)
		}
	}
	for i := 0; i+1 < n; i++ {
		if err := g.AddEdge(nodeKey(i), nodeKey(i+1), float64(i)); err != nil {
			t.Fatalf("AddEdge %d: %v", i, err)
		}
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	return g, cs
}

func nodeKey(i int) string { return "n" + itoa(i) }

// recoverFromSimDisk reopens the full stack (snapshot + WAL) from disk through
// the real recovery core over the in-memory backend, and returns the recovered
// graph order. A nil WAL is fine: recovery tolerates a snapshot-only directory.
func recoverFromSimDisk(t *testing.T, disk *SimDisk, dir string) int {
	t.Helper()
	res, err := recovery.OpenFS[string, float64](
		simRecoveryFS{disk: disk}, dir,
		recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()},
	)
	if err != nil {
		t.Fatalf("recovery.OpenFS: %v", err)
	}
	return int(res.Graph.LiveOrder())
}

// writeSnapshotTo publishes a self-sufficient snapshot of g to <dir>/snapshot
// through the SimDisk snapshot seam, mirroring exactly what the checkpointer
// does in phase 2.
func writeSnapshotTo(t *testing.T, disk *SimDisk, dir string, g *lpg.Graph[string, float64], cs *csr.CSR[float64]) {
	t.Helper()
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		simSnapshotFS{disk: disk}, dir+"/snapshot", cs, g, txn.NewStringCodec(), nil,
	); err != nil {
		t.Fatalf("WriteSnapshotFullWithMapperCodecAndConstraintsFS: %v", err)
	}
}

// TestFullStack_SnapshotPublishSurvivesCleanCrash is the baseline: a snapshot
// published with the full fsync protocol (staging dir fsync -> rename -> parent
// fsync) survives a crash and recovers to the same order. This is the
// "fsync present => data survives" arm the guard test pairs against.
func TestFullStack_SnapshotPublishSurvivesCleanCrash(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0) // no data faults: isolate the dirent model
	g, cs := buildSnapshotGraph(t, 6)
	writeSnapshotTo(t, disk, "db", g, cs)

	// Crash: revoke any not-yet-fsync'd dirents. The publish completed both its
	// staging-dir and parent-dir fsyncs, so nothing should be revoked.
	disk.Crash()

	if got := recoverFromSimDisk(t, disk, "db"); got != 6 {
		t.Fatalf("clean publish lost data across crash: recovered order=%d, want 6", got)
	}
}

// TestFullStack_SnapshotRepublishCrashPromotesBackup drives the publish of a
// SECOND snapshot (archive live->.bak, rename staging->live) and crashes in the
// window after the staging->live rename but before that rename's parent fsync.
// The new live directory's dirent is not yet durable, so the crash drops it; the
// archived .bak is the only surviving copy, and recovery must promote it. This
// exercises the snapshot publish + recovery snapshot-promote boundary.
func TestFullStack_SnapshotRepublishCrashPromotesBackup(t *testing.T) {
	disk := NewSimDisk(NewSeed(2), 0)

	// First snapshot: 4 nodes, fully published & durable.
	g1, cs1 := buildSnapshotGraph(t, 4)
	writeSnapshotTo(t, disk, "db", g1, cs1)

	// Second snapshot publish, intercepted at the crash window: we replicate the
	// publish steps manually so we can crash between the rename and the parent
	// fsync. Write the staging tree, fsync it, archive live -> .bak, rename
	// staging -> live, then CRASH before ParentDirSync(live).
	g2, cs2 := buildSnapshotGraph(t, 9)
	sfs := simSnapshotFS{disk: disk}
	// Write the new snapshot into a separate staging directory, fully fsync'd.
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		sfs, "db/stage/snapshot", cs2, g2, txn.NewStringCodec(), nil,
	); err != nil {
		t.Fatalf("stage write: %v", err)
	}
	// Archive the live snapshot to .bak (durable rename: fsync parent after).
	if err := disk.Rename("db/snapshot", "db/snapshot.bak"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := disk.ParentDirSync("db/snapshot.bak"); err != nil {
		t.Fatalf("archive parent fsync: %v", err)
	}
	// Publish: rename staged snapshot dir onto the live name, then CRASH before
	// the parent fsync that would make the live dirent durable.
	if err := disk.Rename("db/stage/snapshot", "db/snapshot"); err != nil {
		t.Fatalf("publish rename: %v", err)
	}
	disk.Crash() // lose the not-yet-durable live dirent.

	// The live snapshot is gone; recovery must promote .bak (the 4-node graph)
	// and lose nothing committed before the interrupted republish.
	if got := recoverFromSimDisk(t, disk, "db"); got != 4 {
		t.Fatalf("interrupted republish: recovered order=%d, want 4 (promoted backup)", got)
	}
}

// TestFullStack_GuardDirSyncIsLoadBearing is the non-vacuity guard: it proves the
// staging-directory fsync in the snapshot publish protocol is load-bearing. With
// the real backend (DirSync honoured) a clean publish survives a crash; with a
// backend whose DirSync is neutered (modelling a removed dirFsync call) the SAME
// publish + crash loses the snapshot — so the crash tests above are not vacuous.
func TestFullStack_GuardDirSyncIsLoadBearing(t *testing.T) {
	g, cs := buildSnapshotGraph(t, 5)

	// Arm 1: real backend. DirSync makes the staging children durable before the
	// publish rename, so the published snapshot survives the crash.
	{
		disk := NewSimDisk(NewSeed(3), 0)
		writeSnapshotTo(t, disk, "db", g, cs)
		disk.Crash()
		if got := recoverFromSimDisk(t, disk, "db"); got != 5 {
			t.Fatalf("guard arm 1 (fsync honoured): recovered order=%d, want 5", got)
		}
	}

	// Arm 2: same publish, but the staging-dir fsync is a no-op (the defect a
	// removed dirFsync would introduce). The staging children never become
	// durable, so after the publish rename + crash the published snapshot
	// directory is empty/torn and recovery cannot load it.
	{
		disk := NewSimDisk(NewSeed(3), 0)
		nofsync := noDirSyncSnapshotFS{simSnapshotFS{disk: disk}}
		if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
			nofsync, "db/snapshot", cs, g, txn.NewStringCodec(), nil,
		); err != nil {
			t.Fatalf("guard arm 2 publish: %v", err)
		}
		disk.Crash()
		// The snapshot's component dirents were never fsync'd, so the crash
		// revoked them: recovery must NOT find a loadable 5-node snapshot. A
		// recovered order of 5 here would mean the fsync was NOT load-bearing,
		// i.e. the crash tests are vacuous.
		got := recoverOrderTolerant(t, disk, "db")
		if got == 5 {
			t.Fatal("guard arm 2 (fsync neutered): snapshot survived without the staging-dir fsync — the durability fsync is NOT load-bearing and the crash tests are vacuous")
		}
	}
}

// recoverOrderTolerant recovers from the disk and returns the order, treating a
// snapshot-corruption error as "snapshot did not survive" (order 0). The guard's
// arm 2 expects either a clean empty recovery or a corruption fail-stop; both
// mean the un-fsync'd snapshot did not survive, which is the point.
func recoverOrderTolerant(t *testing.T, disk *SimDisk, dir string) int {
	t.Helper()
	res, err := recovery.OpenFS[string, float64](
		simRecoveryFS{disk: disk}, dir,
		recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()},
	)
	if err != nil {
		if errors.Is(err, snapshot.ErrCorrupted) || errors.Is(err, snapshot.ErrManifestCorrupted) {
			return 0
		}
		// A missing/empty snapshot recovers cleanly to an empty graph; any other
		// error is unexpected.
		t.Fatalf("guard recovery: unexpected error: %v", err)
	}
	return int(res.Graph.LiveOrder())
}

// noDirSyncSnapshotFS wraps simSnapshotFS but turns DirSync into a no-op,
// modelling a snapshot publish path from which a dirFsync call was removed. Its
// ParentDirSync is likewise neutered so neither the staging-dir fsync nor the
// post-rename parent fsync makes any dirent durable.
type noDirSyncSnapshotFS struct{ simSnapshotFS }

func (noDirSyncSnapshotFS) DirSync(string) error       { return nil }
func (noDirSyncSnapshotFS) ParentDirSync(string) error { return nil }
