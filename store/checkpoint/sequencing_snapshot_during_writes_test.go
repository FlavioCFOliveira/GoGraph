package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCheckpoint_SnapshotDuringWrites verifies that ops acked before a
// snapshot and ops acked after are both visible after recovery.
//
// Sequence:
//  1. Commit 3 pre-checkpoint edges via txn.
//  2. Write a full snapshot (WriteSnapshotFull) at the current graph state.
//  3. Commit 2 post-checkpoint edges via txn and sync the WAL.
//  4. Close WAL.
//  5. recovery.OpenString → assert all 5 edges present.
//
// The test uses snapshot.WriteSnapshotFull (v2/v3) so that the mapper
// is persisted and recovery can reconstruct all string-keyed edges
// without relying on WAL replay for the pre-snapshot portion.
func TestCheckpoint_SnapshotDuringWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// Use NewStore (no WeightCodec) so every AddEdge writes an OpAddEdge
	// frame (not OpAddEdgeWeighted). recovery.OpenString can replay OpAddEdge
	// frames via its legacy codec path; OpAddEdgeWeighted frames with a nil
	// WeightCodec are silently dropped.
	store := txn.NewStore(g, w)

	// Phase 1: 3 pre-checkpoint edges.
	preEdges := make([][2]string, 0, 5)
	preEdges = append(preEdges, [2]string{"x", "y"}, [2]string{"y", "z"}, [2]string{"z", "x"})
	for _, e := range preEdges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge pre %s->%s: %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit pre: %v", err)
		}
	}

	// Phase 2: full snapshot at this point (v2/v3 — includes mapper.bin).
	snapDir := filepath.Join(dir, "snapshot")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(snapDir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Create and start a checkpointer so goleak is satisfied (this
	// package's TestMain runs goleak). We do not use it actively here;
	// the snapshot was written directly above. We start and immediately
	// stop it to keep the goroutine lifecycle clean.
	var mu sync.Mutex
	cp := New[string, int64](Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)
	cp.Stop()
	cancel()

	// Phase 3: 2 post-checkpoint edges appended to the WAL.
	postEdges := [][2]string{
		{"p", "q"}, {"q", "r"},
	}
	for _, e := range postEdges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge post %s->%s: %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit post: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("WAL Sync: %v", err)
	}

	// Phase 4: close WAL (deferred above, but Close here for clarity in
	// the failure message if recovery cannot open it).
	// We rely on the defer above to close — just flush.

	// Phase 5: recovery using the v2/v3 snapshot + WAL tail.
	res, err := recovery.OpenString(dir)
	if err != nil {
		t.Fatalf("recovery.OpenString: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false")
	}

	// All 5 edges (3 pre + 2 post) must be present.
	allEdges := make([][2]string, 0, len(preEdges)+len(postEdges))
	allEdges = append(allEdges, preEdges...)
	allEdges = append(allEdges, postEdges...)
	for _, e := range allEdges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("edge %s->%s missing from recovered graph", e[0], e[1])
		}
	}

	// Order check: at least 5 distinct nodes.
	if got := res.Graph.AdjList().Order(); got < 5 {
		t.Fatalf("recovered graph Order = %d, want >= 5", got)
	}
}
