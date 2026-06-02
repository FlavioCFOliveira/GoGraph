package recovery

// edge_handle_durable_test.go — recovery coverage for the Stage-2
// handle-bearing WAL op kinds (OpAddEdgeH, OpSetEdgeLabelByHandle,
// OpSetEdgePropertyByHandle, OpRemoveEdgeInstanceByHandle): a replayed
// transaction must rebuild each parallel edge keyed to its stable handle,
// idempotently against a duplicate add (the snapshot+WAL overlap), and must
// re-seed the handle high-water counter (invariant I5).
//
// Layer: short.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func edgeHandleOpts() Options[string, float64] {
	return Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// TestRecovery_HandleOps_RebuildsPerHandleType writes two distinctly typed
// parallel edges through the handle-bearing Tx API, replays the WAL into a
// fresh graph, and asserts each edge keeps its own type keyed to its handle.
func TestRecovery_HandleOps_RebuildsPerHandleType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	const h1 uint64 = 1
	const h2 uint64 = 2
	tx := s.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.AddEdgeWithHandle("x", "y", 1, h1))
	mustTx(t, tx.SetEdgeLabelByHandle("x", "y", h1, "USES"))
	mustTx(t, tx.SetEdgePropertyByHandle("x", "y", h1, "w", lpg.Int64Value(7)))
	mustTx(t, tx.AddEdgeWithHandle("x", "y", 1, h2))
	mustTx(t, tx.SetEdgeLabelByHandle("x", "y", h2, "CALLS"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, edgeHandleOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rg := res.Graph
	if got := rg.EdgeLabelsByHandle("x", "y", h1); len(got) != 1 || got[0] != "USES" {
		t.Fatalf("recovered handle h1 type = %v, want [USES]", got)
	}
	if got := rg.EdgeLabelsByHandle("x", "y", h2); len(got) != 1 || got[0] != "CALLS" {
		t.Fatalf("recovered handle h2 type = %v, want [CALLS]", got)
	}
	// Two parallel edges must exist.
	srcID, _ := rg.AdjList().Mapper().Lookup("x")
	nbs, _, _ := rg.AdjList().LoadEntryH(srcID)
	if len(nbs) != 2 {
		t.Fatalf("recovered parallel edge count = %d, want 2", len(nbs))
	}
	// I5: a post-recovery handle must exceed the max replayed handle.
	if next := rg.NextEdgeHandle(); next <= h2 {
		t.Fatalf("post-recovery NextEdgeHandle = %d, want > %d (high-water not seeded)", next, h2)
	}
}

// TestRecovery_HandleOps_DuplicateAddIdempotent replays a WAL that adds the
// SAME handle twice (modelling a snapshot + full-WAL overlap where the
// snapshot already loaded the edge) and asserts no doubled parallel edge.
func TestRecovery_HandleOps_DuplicateAddIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	const h uint64 = 5
	// First transaction adds the edge with handle h.
	tx := s.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.AddEdgeWithHandle("x", "y", 1, h))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	// Second transaction re-adds the SAME handle (overlap) — must be a no-op
	// on replay.
	tx = s.Begin()
	mustTx(t, tx.AddEdgeWithHandle("x", "y", 1, h))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, edgeHandleOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	srcID, _ := res.Graph.AdjList().Mapper().Lookup("x")
	nbs, _, _ := res.Graph.AdjList().LoadEntryH(srcID)
	if len(nbs) != 1 {
		t.Fatalf("recovered edge count = %d, want 1 (idempotent re-add of same handle)", len(nbs))
	}
}

// TestRecovery_HandleOps_RemoveInstance confirms an OpRemoveEdgeInstanceByHandle
// frame drops a handle's per-CREATE metadata on replay.
func TestRecovery_HandleOps_RemoveInstance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	const h uint64 = 9
	tx := s.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.AddEdgeWithHandle("x", "y", 1, h))
	mustTx(t, tx.SetEdgeLabelByHandle("x", "y", h, "T"))
	mustTx(t, tx.RemoveEdgeInstanceByHandle("x", "y", h))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, edgeHandleOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := res.Graph.EdgeLabelsByHandle("x", "y", h); got != nil {
		t.Fatalf("handle metadata survived RemoveEdgeInstanceByHandle: %v", got)
	}
}
