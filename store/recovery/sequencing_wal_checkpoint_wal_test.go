package recovery

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/checkpoint"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestSequencing_WALCheckpointWAL verifies the three-phase
// write→checkpoint→write→recovery sequence:
//
//  1. Write 5 edges via txn commits to the WAL.
//  2. Trigger a checkpoint (writes a self-sufficient snapshot, truncates WAL).
//  3. Write 3 more edges via txn commits to the truncated WAL.
//  4. Close WAL and checkpointer.
//  5. Call recovery.OpenString and assert that the graph contains ALL
//     8 distinct edges (5 pre-checkpoint + 3 post-checkpoint).
//
// The checkpointer now writes a self-sufficient v3 snapshot for
// string-keyed graphs (WriteSnapshotFull: CSR + labels + properties +
// mapper.bin). Because the snapshot carries the NodeID→string mapper,
// the 5 pre-checkpoint edges survive truncation and are reconstructed
// from the snapshot ALONE; the 3 post-checkpoint edges are then layered
// on by replaying the post-checkpoint WAL tail. This is the Durability
// fix for audit gap F2 (docs/acid-audit.md): before the fix the
// checkpoint wrote a v1 CSR-only snapshot and the 5 pre-checkpoint edges
// were permanently lost — a committed transaction did not survive a
// checkpoint.
//
// We therefore assert:
//   - SnapshotHit == true
//   - All 8 edges (5 pre-checkpoint + 3 post-checkpoint) are present.
func TestSequencing_WALCheckpointWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)

	// Phase 1: 5 pre-checkpoint edges.
	preEdges := [][2]string{
		{"n0", "n1"}, {"n1", "n2"}, {"n2", "n3"}, {"n3", "n4"}, {"n4", "n0"},
	}
	for _, e := range preEdges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Phase 2: checkpoint.
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Phase 3: 3 post-checkpoint edges.
	postEdges := [][2]string{
		{"p0", "p1"}, {"p1", "p2"}, {"p2", "p0"},
	}
	for _, e := range postEdges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Phase 4: teardown.
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Phase 5: recovery.
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}

	// WALOps must account for the 3 post-checkpoint AddEdge commits.
	// Each AddEdge via txn.NewStore writes one v1 WAL frame.
	if res.WALOps < len(postEdges) {
		t.Fatalf("WALOps = %d, want >= %d (post-checkpoint edges)", res.WALOps, len(postEdges))
	}

	// Durability (F2): ALL 8 edges must survive — the 5 pre-checkpoint
	// edges from the self-sufficient snapshot and the 3 post-checkpoint
	// edges from the WAL tail replay.
	for _, e := range preEdges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("pre-checkpoint edge %s->%s missing — committed data lost across checkpoint", e[0], e[1])
		}
	}
	for _, e := range postEdges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("post-checkpoint edge %s->%s missing from recovered graph", e[0], e[1])
		}
	}
}
