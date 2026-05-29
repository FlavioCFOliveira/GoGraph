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

// TestCheckpointDurability_LabelsPropertiesEdgesSurvive is the F2
// regression test (see docs/acid-audit.md). It proves that a committed
// transaction's edges, node labels, node properties, and edge properties
// all SURVIVE a checkpoint that truncates the WAL — the Durability
// guarantee the legacy CSR-only checkpoint violated.
//
// Before the F2 fix the checkpointer wrote a v1 CSR-only snapshot and
// truncated the WAL, so on recovery the labels/properties were gone and
// (because a v1 snapshot has no mapper.bin) even the adjacency could not
// be restored — recovery yielded an empty graph. After the fix the
// checkpoint writes a self-sufficient v3 snapshot (CSR + labels +
// properties + mapper) before truncation, so the entire committed state
// is reconstructable from the snapshot ALONE.
//
// The test asserts WALOps == 0 after recovery: every byte of state came
// from the snapshot, proving the snapshot is genuinely self-sufficient
// and the truncation lost nothing.
func TestCheckpointDurability_LabelsPropertiesEdgesSurvive(t *testing.T) {
	t.Parallel()
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
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// One rich multi-op transaction: two labelled, propertied nodes joined
	// by a weighted, propertied edge.
	tx := store.Begin()
	mustTx(t, tx.AddNode("alice"))
	mustTx(t, tx.SetNodeLabel("alice", "Person"))
	mustTx(t, tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice")))
	mustTx(t, tx.SetNodeProperty("alice", "age", lpg.Int64Value(30)))
	mustTx(t, tx.AddNode("bob"))
	mustTx(t, tx.SetNodeLabel("bob", "Person"))
	mustTx(t, tx.SetNodeProperty("bob", "name", lpg.StringValue("Bob")))
	mustTx(t, tx.AddEdge("alice", "bob", 7))
	mustTx(t, tx.SetEdgeLabel("alice", "bob", "KNOWS"))
	mustTx(t, tx.SetEdgeProperty("alice", "bob", "since", lpg.Int64Value(2020)))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Checkpoint: writes a self-sufficient snapshot then truncates the WAL.
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recover from the directory. The graph must be fully reconstructed
	// from the snapshot alone (WALOps == 0).
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (snapshot must be self-sufficient after truncation)", res.WALOps)
	}
	rg := res.Graph

	// Edge survives.
	if !rg.AdjList().HasEdge("alice", "bob") {
		t.Error("edge alice->bob missing after checkpoint+recovery")
	}
	// Node labels survive.
	if !rg.HasNodeLabel("alice", "Person") {
		t.Error("node label Person on alice missing after checkpoint+recovery")
	}
	if !rg.HasNodeLabel("bob", "Person") {
		t.Error("node label Person on bob missing after checkpoint+recovery")
	}
	// Node properties survive with their values.
	assertStringProp(t, rg, "alice", "name", "Alice")
	assertInt64Prop(t, rg, "alice", "age", 30)
	assertStringProp(t, rg, "bob", "name", "Bob")
	// Edge property survives.
	if v, ok := rg.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Error("edge property since missing after checkpoint+recovery")
	} else if got, _ := v.Int64(); got != 2020 {
		t.Errorf("edge property since = %d, want 2020", got)
	}
}

func mustTx(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tx op: %v", err)
	}
}

func assertStringProp(t *testing.T, g *lpg.Graph[string, int64], node, key, want string) {
	t.Helper()
	v, ok := g.GetNodeProperty(node, key)
	if !ok {
		t.Errorf("node property %s.%s missing after recovery", node, key)
		return
	}
	got, _ := v.String()
	if got != want {
		t.Errorf("node property %s.%s = %q, want %q", node, key, got, want)
	}
}

func assertInt64Prop(t *testing.T, g *lpg.Graph[string, int64], node, key string, want int64) {
	t.Helper()
	v, ok := g.GetNodeProperty(node, key)
	if !ok {
		t.Errorf("node property %s.%s missing after recovery", node, key)
		return
	}
	got, _ := v.Int64()
	if got != want {
		t.Errorf("node property %s.%s = %d, want %d", node, key, got, want)
	}
}
