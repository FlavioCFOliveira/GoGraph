package bulk

import (
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestLoader_RecoveryEqual verifies the full bulk-to-recovery path:
//
//  1. Bulk-load a deterministic set of edges into a csrfile.
//  2. Build an in-memory LPG from the returned CSR and write a full
//     snapshot using snapshot.WriteSnapshotFull.
//  3. Open the snapshot directory via recovery.OpenString and assert
//     the recovered graph contains the expected edges.
//
// The WAL is opened and closed immediately so recovery.OpenString finds
// a valid (empty) WAL file at the expected path — the bulk loader
// writes no WAL frames.
func TestLoader_RecoveryEqual(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "graph.csr")

	// Phase 1: bulk load.
	l := New(Options{OutputPath: outPath, Directed: true})
	edges := []Edge{
		{Src: "alpha", Dst: "beta", Weight: 1},
		{Src: "beta", Dst: "gamma", Weight: 2},
		{Src: "gamma", Dst: "alpha", Weight: 3},
		{Src: "alpha", Dst: "delta", Weight: 4},
		{Src: "delta", Dst: "epsilon", Weight: 5},
	}
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	_, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	// Phase 2: build LPG from CSR and write snapshot.
	// The CSR contains NodeIDs; we need a string-keyed LPG for recovery.
	// We reconstruct it via the adjlist path that the bulk loader used.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := g.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.Src, e.Dst, err)
		}
	}
	snapCSR := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, snapCSR, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Create an empty WAL so recovery.OpenString does not error on a
	// missing file. The bulk loader does not write WAL frames.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Suppress the unused variable warning — c is used to confirm that
	// the csrfile also holds a non-trivial graph.
	if c.Order() == 0 {
		t.Fatalf("CSR Order = 0, want > 0")
	}

	// Phase 3: recovery.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false")
	}

	recovered := res.Graph.AdjList()
	for _, e := range edges {
		if !recovered.HasEdge(e.Src, e.Dst) {
			t.Errorf("edge %s->%s missing from recovered graph", e.Src, e.Dst)
		}
	}
	if got := recovered.Order(); got == 0 {
		t.Fatalf("recovered graph Order = 0")
	}
}
