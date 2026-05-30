package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCheckpoint_CrashMidSimulation models a crash that occurs after
// a new snapshot staging directory (snapshot.tmp) is created but
// before the atomic rename to snapshot/ completes.
//
// Setup:
//  1. Commit edge "a"→"b" through txn.Store (WAL frame + in-memory).
//  2. Force a checkpoint (checkpoint A): writes v1 CSR snapshot and
//     truncates the WAL.
//  3. Commit 3 more edges via txn.Store (WAL tail after checkpoint A).
//  4. Create snapshot.tmp as a sibling of snapshot/ to simulate a crash
//     mid-rename (the write started but never reached os.Rename).
//
// After the simulated crash, recovery.Open must:
//   - Find snapshot A (SnapshotHit = true).
//   - Replay the 3 post-checkpoint WAL frames (WALOps = 3).
//   - Return no error.
//
// NOTE: v1 CSR snapshots do not carry mapper.bin, so the graph state
// from snapshot A is NOT applied to the recovered graph topology (only
// WAL frames produce HasEdge hits). The test therefore verifies the
// WAL replay count rather than specific edge presence.
func TestCheckpoint_CrashMidSimulation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// Open WAL and in-memory graph.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())

	// Phase 1: commit one edge so the WAL is non-empty before checkpoint.
	tx := store.Begin()
	if err := tx.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge(a->b): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Phase 2: checkpoint A — writes snapshot/, truncates WAL.
	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	if err := cp.Trigger(); err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()

	if cp.Stats().Checkpoints < 1 {
		t.Fatalf("expected at least one checkpoint")
	}

	// Phase 3: 3 more edges committed to the WAL after checkpoint A.
	postEdges := [][2]string{{"b", "c"}, {"c", "d"}, {"d", "e"}}
	for _, e := range postEdges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%s->%s): %v", e[0], e[1], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Phase 4: simulate crash mid-checkpoint by creating snapshot.tmp
	// without renaming it to snapshot/. The next checkpoint would have
	// started writing here but the process died before os.Rename.
	snapTmp := filepath.Join(dir, "snapshot.tmp")
	if err := os.MkdirAll(snapTmp, 0o750); err != nil {
		t.Fatalf("mkdir snapshot.tmp: %v", err)
	}

	// Recovery must load snapshot A + replay the 3 post-checkpoint WAL frames.
	// snapshot.tmp is not a valid snapshot directory (no manifest.json), so
	// recovery.Open should ignore it and continue with snapshot/.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit = false: snapshot A should be present at %s/snapshot",
			dir)
	}
	// The post-checkpoint v3 WAL frames are replayed by the typed open path.
	if res.WALOps != len(postEdges) {
		t.Fatalf("WALOps = %d, want %d (post-checkpoint edges)", res.WALOps, len(postEdges))
	}
	// Post-checkpoint edges must be in the recovered graph.
	for _, e := range postEdges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("HasEdge(%s->%s) = false after recovery", e[0], e[1])
		}
	}
}
