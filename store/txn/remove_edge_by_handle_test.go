package txn_test

// remove_edge_by_handle_test.go — round-trip coverage for the
// OpRemoveEdgeByHandle op kind (rmp #2018). It exercises both the live commit
// apply path (txn.applyOp) and the WAL recovery replay path
// (recovery.applyOpCodec) in the default (untagged) test layer, complementing
// the crash-injection durability proof in store/recovery.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// countParallel returns how many adjacency slots (and their handles) exist from
// src to dst in g.
func countParallel(t *testing.T, g *lpg.Graph[string, int64], src, dst string) (n int, handles []uint64) {
	t.Helper()
	srcID, ok := g.AdjList().Mapper().Lookup(src)
	if !ok {
		return 0, nil
	}
	dstID, ok := g.AdjList().Mapper().Lookup(dst)
	if !ok {
		return 0, nil
	}
	nbs, _, hs := g.AdjList().LoadEntryH(srcID)
	for i, nb := range nbs {
		if nb == dstID {
			n++
			handles = append(handles, hs[i])
		}
	}
	return n, handles
}

// TestRoundtrip_RemoveEdgeByHandle_LiveAndRecovered commits two parallel edges
// (handles 10 and 20) then durably removes the SECOND (handle 20) via
// Tx.RemoveEdgeByHandle. It asserts the exact instance is gone in BOTH the live
// in-memory graph (proving txn.applyOp) and the WAL-recovered graph (proving
// recovery.applyOpCodec).
func TestRoundtrip_RemoveEdgeByHandle_LiveAndRecovered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Tx 1 — two parallel edges with distinct stable handles.
	tx := s.Begin()
	if e := tx.AddEdgeWithHandle("a", "b", 1, 10); e != nil {
		t.Fatalf("AddEdgeWithHandle h10: %v", e)
	}
	if e := tx.AddEdgeWithHandle("a", "b", 2, 20); e != nil {
		t.Fatalf("AddEdgeWithHandle h20: %v", e)
	}
	if e := tx.Commit(); e != nil {
		t.Fatalf("Commit(build): %v", e)
	}

	// Tx 2 — remove the second parallel instance (handle 20) by handle.
	tx2 := s.Begin()
	if e := tx2.RemoveEdgeByHandle("a", "b", 20); e != nil {
		t.Fatalf("RemoveEdgeByHandle: %v", e)
	}
	if e := tx2.Commit(); e != nil {
		t.Fatalf("Commit(delete): %v", e)
	}

	// Live graph (txn.applyOp): exactly one slot survives, carrying handle 10.
	if n, handles := countParallel(t, g, "a", "b"); n != 1 || handles[0] != 10 {
		t.Fatalf("live graph: %d parallel slots handles=%v, want 1 slot handle 10", n, handles)
	}

	if e := w.Close(); e != nil {
		t.Fatalf("wal.Close: %v", e)
	}

	// Recovered graph (recovery.applyOpCodec): same instance-precise result.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if n, handles := countParallel(t, res.Graph, "a", "b"); n != 1 || handles[0] != 10 {
		t.Fatalf("recovered graph: %d parallel slots handles=%v, want 1 slot handle 10", n, handles)
	}
}
