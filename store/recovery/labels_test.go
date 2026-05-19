package recovery

import (
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_LabelsSurviveRestart wires the full v2 snapshot path
// into a transactional flow: the test writes a graph with labels via
// txn.NewStore (WAL-driven), then explicitly persists a v2 snapshot
// alongside the WAL via snapshot.WriteSnapshotFull, then calls
// recovery.OpenString to simulate a restart. The recovered graph
// must carry every node and edge label that was committed before the
// snapshot.
//
// Note that this test exercises the snapshot+WAL composition the
// way a production restart sees it: the WAL replay rebuilds the
// mapper (and re-asserts the labels), and the snapshot label apply
// runs after replay (idempotent here, but the key path verified by
// the test).
//
//nolint:gocyclo // test: per-commit assertions across node and edge labels
func TestRecovery_LabelsSurviveRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)

	commits := []struct{ src, dst, nodeLabel, edgeLabel string }{
		{"alice", "bob", "Person", "KNOWS"},
		{"bob", "carol", "Person", "KNOWS"},
		{"carol", "dave", "Person", "FOLLOWS"},
	}
	for _, c := range commits {
		tx := store.Begin()
		if err := tx.SetNodeLabel(c.src, c.nodeLabel); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := tx.SetNodeLabel(c.dst, c.nodeLabel); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := tx.AddEdge(c.src, c.dst, 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.SetEdgeLabel(c.src, c.dst, c.edgeLabel); err != nil {
			t.Fatalf("SetEdgeLabel: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	// Take a v2 snapshot of the current state (CSR + labels.bin).
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit = false, want true")
	}
	if res.SnapshotLabels == 0 {
		t.Fatalf("SnapshotLabels = 0, want > 0 after applying v2 labels.bin")
	}
	// Every committed edge with its label must survive.
	for _, c := range commits {
		if !res.Graph.AdjList().HasEdge(c.src, c.dst) {
			t.Errorf("HasEdge(%q,%q) = false", c.src, c.dst)
		}
		if !res.Graph.HasNodeLabel(c.src, c.nodeLabel) {
			t.Errorf("HasNodeLabel(%q,%q) = false", c.src, c.nodeLabel)
		}
		if !res.Graph.HasNodeLabel(c.dst, c.nodeLabel) {
			t.Errorf("HasNodeLabel(%q,%q) = false", c.dst, c.nodeLabel)
		}
		if !res.Graph.HasEdgeLabel(c.src, c.dst, c.edgeLabel) {
			t.Errorf("HasEdgeLabel(%q,%q,%q) = false", c.src, c.dst, c.edgeLabel)
		}
	}
}

// TestRecovery_V1SnapshotStillRecovers asserts that an old v1
// snapshot (csr.bin only) coexisting with the WAL continues to load
// cleanly via OpenString. SnapshotHit is true and SnapshotLabels is
// 0 — exactly the forward-compat contract.
func TestRecovery_V1SnapshotStillRecovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)
	tx := store.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	// Write a v1-shaped snapshot (no labels.bin) via the legacy path.
	if err := snapshot.WriteSnapshotCSR(snapDir, cs); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit = false")
	}
	if res.SnapshotLabels != 0 {
		t.Fatalf("SnapshotLabels = %d, want 0 (v1 snapshot has no labels.bin)", res.SnapshotLabels)
	}
	// Labels still survive via WAL replay alone.
	if !res.Graph.HasNodeLabel("alice", "Person") {
		t.Fatalf("alice should carry Person via WAL replay")
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatalf("alice -> bob missing")
	}
}
