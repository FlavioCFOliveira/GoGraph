package recovery

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_SnapshotPlusWALReplay builds a graph with N=8 nodes
// committed before the snapshot and M=6 additional nodes committed
// after the snapshot but before WAL close. The recovered graph must
// carry N+M nodes and both the snapshot topology and the WAL-replay
// topology intact.
//
// The test is distinct from [TestRecovery_V3Snapshot_WALReplayAfterSnapshot]
// which uses N=2 pre-snapshot nodes and M=1 post-snapshot node. This
// variant uses larger N and M to stress the mapper restoration path
// that must Intern snapshot keys before replaying WAL frames that
// may reference both old and new keys.
//
//nolint:gocyclo // test: N pre-snapshot ops + M post-snapshot ops + full assertion loop
func TestRecovery_SnapshotPlusWALReplay(t *testing.T) {
	t.Parallel()

	const N = 8 // pre-snapshot committed nodes
	const M = 6 // post-snapshot WAL-only nodes

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Phase 1: N pre-snapshot nodes, ring topology.
	preNodes := make([]string, N)
	for i := range preNodes {
		preNodes[i] = "pre" + itoa(i)
	}
	for i, src := range preNodes {
		dst := preNodes[(i+1)%N]
		tx := s.Begin()
		if err := tx.AddEdge(src, dst, int64(i)); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", src, dst, err)
		}
		if err := tx.SetNodeLabel(src, "Pre"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Phase 2: snapshot at this point (N nodes, N edges).
	c := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Phase 3: M post-snapshot nodes appended to the WAL.
	postNodes := make([]string, M)
	for i := range postNodes {
		postNodes[i] = "post" + itoa(i)
	}
	for i, src := range postNodes {
		dst := postNodes[(i+1)%M]
		tx := s.Begin()
		if err := tx.AddEdge(src, dst, int64(100+i)); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", src, dst, err)
		}
		if err := tx.SetNodeLabel(src, "Post"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Also add a cross-boundary edge: pre0 -> post0.
	txCross := s.Begin()
	if err := txCross.AddEdge(preNodes[0], postNodes[0], int64(999)); err != nil {
		t.Fatalf("AddEdge(cross): %v", err)
	}
	if err := txCross.Commit(); err != nil {
		t.Fatalf("Commit(cross): %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Phase 4: recovery.
	res, err := Open[string, int64](dir, Options[string, int64](opts))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}

	// All N pre-snapshot nodes must have their ring edges and labels.
	for i, src := range preNodes {
		dst := preNodes[(i+1)%N]
		if !res.Graph.AdjList().HasEdge(src, dst) {
			t.Errorf("pre-snapshot edge %s->%s missing", src, dst)
		}
		if !res.Graph.HasNodeLabel(src, "Pre") {
			t.Errorf("pre-snapshot node %s missing label Pre", src)
		}
	}

	// All M post-snapshot nodes must have their ring edges and labels.
	for i, src := range postNodes {
		dst := postNodes[(i+1)%M]
		if !res.Graph.AdjList().HasEdge(src, dst) {
			t.Errorf("post-snapshot edge %s->%s missing", src, dst)
		}
		if !res.Graph.HasNodeLabel(src, "Post") {
			t.Errorf("post-snapshot node %s missing label Post", src)
		}
	}

	// Cross-boundary edge must be present.
	if !res.Graph.AdjList().HasEdge(preNodes[0], postNodes[0]) {
		t.Errorf("cross-boundary edge %s->%s missing", preNodes[0], postNodes[0])
	}

	// Order check: exactly N+M distinct nodes (each pre and post node
	// appears once; the mapper deduplicates by key).
	if got, want := res.Graph.AdjList().Order(), uint64(N+M); got != want {
		t.Fatalf("Order = %d, want %d (N+M)", got, want)
	}

}
