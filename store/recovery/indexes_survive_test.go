package recovery

import (
	"path/filepath"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/index"
	"gograph/graph/index/btree"
	"gograph/graph/index/hash"
	"gograph/graph/index/label"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_IndexesSurvive asserts that all three supported index
// types (label, hash, btree) survive a snapshot+WAL recovery cycle
// and remain queryable using their respective query APIs after restart.
//
// This test is distinct from [TestRecovery_IndexesSurviveRestart] in
// that it focuses on the post-restart *query path* — not just
// deserialization correctness — by calling each index's dedicated
// query method and verifying the result set.
//
// Index operations exercised:
//   - [label.Index.Has]: label membership query.
//   - [hash.Index.Lookup]: hash bucket membership query.
//   - [btree.Index.Lookup]: b-tree range / key lookup returning a
//     [*roaring64.Bitmap].
//
//nolint:gocyclo // test: build + all-three-index snapshot + restart + per-index query assertions
func TestRecovery_IndexesSurvive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Phase 1: populate three indexes with deterministic (key, NodeID)
	// pairs and snapshot the graph.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)

	labIdx := label.NewIndex()
	hshIdx := hash.New[string]()
	btIdx := btree.New[string]()
	if err := mgr.CreateIndex("idx.label", labIdx); err != nil {
		t.Fatalf("CreateIndex idx.label: %v", err)
	}
	if err := mgr.CreateIndex("idx.hash", hshIdx); err != nil {
		t.Fatalf("CreateIndex idx.hash: %v", err)
	}
	if err := mgr.CreateIndex("idx.btree", btIdx); err != nil {
		t.Fatalf("CreateIndex idx.btree: %v", err)
	}

	// Commit a set of nodes via the WAL so the mapper is populated.
	store := txn.NewStore(g, w)
	nodeNames := []string{"alice", "bob", "carol", "dave", "eve"}
	for i := 0; i < len(nodeNames)-1; i++ {
		tx := store.Begin()
		if err := tx.AddEdge(nodeNames[i], nodeNames[i+1], 0); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// Populate each index with known (key, NodeID) tuples. NodeIDs
	// are assigned 0..N-1 in interning order; we use graph.NodeID
	// explicitly to be precise.
	const labelA uint32 = 1
	const labelB uint32 = 2
	// label index: every node is in labelA; even nodes are also in labelB.
	for i := uint64(0); i < uint64(len(nodeNames)); i++ {
		labIdx.Add(labelA, graph.NodeID(i))
		if i%2 == 0 {
			labIdx.Add(labelB, graph.NodeID(i))
		}
	}
	// hash index: keyed by email pattern.
	for i, name := range nodeNames {
		hshIdx.Insert("email:"+name, graph.NodeID(uint64(i)))
	}
	// btree index: keyed by a score string.
	for i, name := range nodeNames {
		btIdx.Insert("score:"+name, graph.NodeID(uint64(i)))
	}

	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: recover. Wire the fresh manager BEFORE loading the
	// snapshot index payload via applySnapshotIndexes.
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}

	freshMgr := index.NewManager()
	res.Graph.SetIndexManager(freshMgr)
	freshLab := label.NewIndex()
	freshHsh := hash.New[string]()
	freshBt := btree.New[string]()
	if err := freshMgr.CreateIndex("idx.label", freshLab); err != nil {
		t.Fatal(err)
	}
	if err := freshMgr.CreateIndex("idx.hash", freshHsh); err != nil {
		t.Fatal(err)
	}
	if err := freshMgr.CreateIndex("idx.btree", freshBt); err != nil {
		t.Fatal(err)
	}

	// Re-apply the snapshot index payload via the internal helper.
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if n := applySnapshotIndexes(freshMgr, loaded.Indexes); n != 3 {
		t.Fatalf("applySnapshotIndexes = %d, want 3", n)
	}

	// Phase 3: query each index using its dedicated API.

	// label.Index.Has — every node must be in labelA.
	for i := uint64(0); i < uint64(len(nodeNames)); i++ {
		if !freshLab.Has(labelA, graph.NodeID(i)) {
			t.Errorf("post-restart: label.Has(labelA, %d) = false, want true", i)
		}
	}
	// Nodes 0, 2, 4 must also be in labelB; 1, 3 must not.
	for i := uint64(0); i < uint64(len(nodeNames)); i++ {
		wantB := i%2 == 0
		if got := freshLab.Has(labelB, graph.NodeID(i)); got != wantB {
			t.Errorf("post-restart: label.Has(labelB, %d) = %v, want %v", i, got, wantB)
		}
	}

	// hash.Index.Lookup — Lookup returns a bitmap; Contains is the
	// per-node membership check.
	for i, name := range nodeNames {
		key := "email:" + name
		if !freshHsh.Contains(key, graph.NodeID(uint64(i))) {
			t.Errorf("post-restart: hash.Contains(%q, %d) = false", key, i)
		}
	}

	// btree.Index.Lookup — Lookup returns a bitmap; verify each entry.
	for i, name := range nodeNames {
		key := "score:" + name
		bm := freshBt.Lookup(key)
		if bm == nil || !bm.Contains(uint64(i)) {
			t.Errorf("post-restart: btree.Lookup(%q) does not contain %d", key, i)
		}
	}

}
