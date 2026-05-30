package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCheckpoint_TransitionRecovery verifies the WAL→snapshot
// transition: after a forced checkpoint the snapshot exists on disk,
// the WAL is truncated, and recovery.Open recognises the
// snapshot directory.
//
// The checkpointer writes a self-sufficient v3 snapshot
// (WriteSnapshotFull with mapper.bin) — the F2 durability fix
// (docs/acid-audit.md). Because the snapshot carries the
// NodeID->key table, recovery.Open reconstructs every string-keyed
// edge from the snapshot alone, reporting SnapshotHit=true and
// WALOps=0 (the WAL was truncated).
func TestCheckpoint_TransitionRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Build a small graph with 5 edges directly in memory.
	// Edges are NOT committed via txn: the checkpoint reads the live
	// in-memory AdjList and writes a CSR snapshot of it.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	edges := [][2]string{
		{"a", "b"}, {"b", "c"}, {"c", "d"}, {"d", "e"}, {"e", "a"},
	}
	for _, e := range edges {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	stats := cp.Stats()
	if stats.Checkpoints < 1 {
		t.Fatalf("Checkpoints = %d, want >= 1", stats.Checkpoints)
	}
	if stats.WALTruncBytes == 0 {
		// An empty WAL still satisfies the Truncate contract: the WAL
		// writer reports 0 freed bytes when the file is already empty.
		// The real invariant is that Checkpoints was incremented and no
		// error was recorded.
		if stats.LastError != "" {
			t.Fatalf("checkpoint error recorded: %s", stats.LastError)
		}
	}

	// Verify snapshot exists and was written by the checkpointer.
	snapDir := filepath.Join(dir, "snapshot")
	loaded, err := snapshot.Open(snapDir)
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	// The checkpointer now writes a self-sufficient v3 snapshot
	// (WriteSnapshotFull with mapper.bin) before truncating the WAL —
	// the F2 durability fix (docs/acid-audit.md).
	if loaded.Manifest.Version != snapshot.ManifestVersion {
		t.Fatalf("manifest version %d, want %d", loaded.Manifest.Version, snapshot.ManifestVersion)
	}
	// 5 edges must be encoded in the CSR edge array.
	if got := len(loaded.CSR.Edges); got != len(edges) {
		t.Fatalf("CSR edge count = %d, want %d", got, len(edges))
	}

	// recovery.Open reconstructs the full string-keyed graph from
	// the self-sufficient snapshot ALONE: mapper.bin restores the
	// NodeID->key table, so all 5 edges are recoverable by key with the
	// WAL truncated (WALOps == 0). Before F2 a v1 CSR-only snapshot could
	// not reconstruct string-keyed edges and they were silently lost.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit = false after checkpoint")
	}
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d", res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (WAL truncated; state from snapshot)", res.WALOps)
	}
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("edge %s->%s not reconstructed from self-sufficient snapshot", e[0], e[1])
		}
	}
}
