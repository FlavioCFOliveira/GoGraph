package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// This file covers the checkpoint publish-before-truncate ordering and the WAL
// prefix-truncate boundary on the in-memory [SimDisk], using a real WAL written
// by a [SimStore] and the SimDisk-backed snapshot seam. The production WAL
// TruncatePrefix is path-backed only (wal.ErrPrefixTruncateUnsupported on the
// path-less OpenWith handle the simulator uses), so the truncate itself is
// modelled here as the WAL-image rewrite the checkpointer's phase 3 performs;
// what the test exercises end to end on SimDisk is the ordering contract and
// recovery across the boundary.

// seedWALGraph creates a SimStore over disk, writes n chained nodes through the
// engine's autocommit write path (each CREATE appends durable WAL ops), and
// returns the live order. The WAL bytes persist in disk after the store is
// crashed.
func seedWALGraph(t *testing.T, disk *SimDisk, n int) int {
	t.Helper()
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	ctx := context.Background()
	adapter := NewEngineAdapter(store.engine)
	for i := 0; i < n; i++ {
		res, werr := adapter.RunWrite(ctx, "CREATE (:N {i:$i})", map[string]any{"i": i})
		if werr != nil {
			t.Fatalf("CREATE %d: %v", i, werr)
		}
		_ = res.Close()
	}
	order := int(store.graph.LiveOrder())
	// Graceful close flushes + fsyncs the WAL so every CREATE is durable in the
	// SimDisk image before we model the checkpoint over it.
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return order
}

// publishSnapshotFromWAL recovers the graph from the current WAL image, builds a
// CSR, and publishes a self-sufficient snapshot of it to <dir>/snapshot through
// the SimDisk snapshot seam — exactly the artefact a checkpoint's phase 2 makes
// durable before it truncates the WAL.
func publishSnapshotFromWAL(t *testing.T, disk *SimDisk, dir string) {
	t.Helper()
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("reopen for snapshot: %v", err)
	}
	cs := csr.BuildFromAdjList(store.graph.AdjList())
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		simSnapshotFS{disk: disk}, dir+"/snapshot", cs, store.graph, txn.NewStringCodec(), nil,
	); err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}
	_ = store.Close()
}

// TestFullStack_CheckpointTruncateBoundary models a COMPLETED checkpoint on
// SimDisk: a self-sufficient snapshot is published and made durable, THEN the
// WAL prefix it folded is truncated (here, the whole WAL, since the snapshot
// covers the full graph). A crash after both steps must recover the full graph
// from the snapshot alone. This exercises the prefix-truncate boundary: recovery
// reconstructs from a snapshot plus a truncated WAL.
func TestFullStack_CheckpointTruncateBoundary(t *testing.T) {
	disk := NewSimDisk(NewSeed(10), 0)
	want := seedWALGraph(t, disk, 7)

	// Phase 2: durable, self-sufficient snapshot.
	publishSnapshotFromWAL(t, disk, "db")
	// Phase 3: the snapshot is durable, so the WAL prefix it folded can be
	// reclaimed. The snapshot covers the entire graph, so the whole WAL is the
	// folded prefix; truncate the image to empty (the suffix is empty).
	if err := disk.TruncatePath("wal", 0); err != nil {
		t.Fatalf("model prefix-truncate: %v", err)
	}
	disk.Crash() // crash after a fully completed checkpoint.

	if got := recoverFromSimDisk(t, disk, "db"); got != want {
		t.Fatalf("post-truncate recovery: order=%d, want %d (snapshot must stand alone)", got, want)
	}
}

// TestFullStack_CheckpointOrderingViolationLosesData proves the
// publish-before-truncate ordering is load-bearing: if the WAL prefix is
// truncated while the snapshot that was meant to fold it is NOT yet durable, a
// crash drops the snapshot AND the WAL — total loss. The correct order (durable
// snapshot first, then truncate, covered above) is what prevents this. The two
// tests together show the ordering, not luck, is what preserves the data.
func TestFullStack_CheckpointOrderingViolationLosesData(t *testing.T) {
	disk := NewSimDisk(NewSeed(11), 0)
	want := seedWALGraph(t, disk, 7)
	if want == 0 {
		t.Fatal("seed produced no nodes")
	}

	// Violation: publish the snapshot into staging but DO NOT make its directory
	// entry durable (skip the publish rename's parent fsync — modelled by never
	// fsyncing), then truncate the WAL anyway.
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	cs := csr.BuildFromAdjList(store.graph.AdjList())
	_ = store.Close()
	// Use the no-fsync backend so neither the staging-dir fsync nor the parent
	// fsync makes the snapshot's dirents durable.
	nofsync := noDirSyncSnapshotFS{simSnapshotFS{disk: disk}}
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		nofsync, "db/snapshot", cs, store.graph, txn.NewStringCodec(), nil,
	); err != nil {
		t.Fatalf("publish (no fsync): %v", err)
	}
	// Truncate the WAL prefix BEFORE the snapshot is durable — the ordering
	// violation.
	if err := disk.TruncatePath("wal", 0); err != nil {
		t.Fatalf("premature truncate: %v", err)
	}
	disk.Crash() // snapshot dirents were never fsync'd -> dropped; WAL is empty.

	got := recoverOrderTolerant(t, disk, "db")
	if got == want {
		t.Fatalf("ordering violation did not lose data: order=%d == want %d; the publish-before-truncate ordering is then not load-bearing", got, want)
	}
}
