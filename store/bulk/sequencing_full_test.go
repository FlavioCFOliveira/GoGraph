package bulk

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestSequencing_BulkCheckpointSnapshotRecovery is a full-pipeline
// integration test covering:
//
//  1. Bulk-load 20 edges into a csrfile.
//  2. Reconstruct an in-memory LPG from the returned CSR edges.
//  3. Open a WAL and create a Checkpointer backed by that LPG.
//  4. Trigger a checkpoint — this writes a snapshot via WriteSnapshotCSR.
//  5. Close WAL and Checkpointer.
//  6. Recover via recovery.Open and verify edge count.
//
// The test does not write any WAL frames; it relies exclusively on the
// snapshot written by the checkpointer. Recovery.SnapshotHit must be
// true and the recovered graph must contain all edges committed by
// the bulk loader.
func TestSequencing_BulkCheckpointSnapshotRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "graph.csr")

	// Phase 1: bulk load 20 deterministic edges.
	edges := make([]Edge, 0, 20)
	nodes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for i, src := range nodes {
		dst := nodes[(i+1)%len(nodes)]
		edges = append(edges, Edge{Src: src, Dst: dst, Weight: int64(i + 1)})
	}
	for i, src := range nodes {
		dst := nodes[(i+2)%len(nodes)]
		edges = append(edges, Edge{Src: src, Dst: dst, Weight: int64(i + 11)})
	}

	l := New(Options{OutputPath: outPath, Directed: true})
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	rows, _, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if rows != len(edges) {
		t.Fatalf("rows = %d, want %d", rows, len(edges))
	}

	// Phase 2: reconstruct LPG from the same edge list.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := g.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.Src, e.Dst, err)
		}
	}

	// Phase 3: open WAL and create checkpointer.
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, int64](
		checkpoint.Config{Dir: dir},
		g, w, &mu,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	// Phase 4: force a checkpoint (writes snapshot).
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Phase 5: recover.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false after checkpoint")
	}

	// The checkpointer now writes a self-sufficient v3 snapshot
	// (WriteSnapshotFull with mapper.bin) — the F2 durability fix
	// (docs/acid-audit.md). All 20 string-keyed edges are reconstructed
	// from the snapshot ALONE: no WAL frames were written, so WALOps == 0.
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d (self-sufficient snapshot)", res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (no WAL frames; state from snapshot)", res.WALOps)
	}
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e.Src, e.Dst) {
			t.Errorf("edge %s->%s not reconstructed from self-sufficient snapshot", e.Src, e.Dst)
		}
	}
	if cp.Stats().Checkpoints < 1 {
		t.Fatalf("Checkpoints = %d, want >= 1", cp.Stats().Checkpoints)
	}
}
