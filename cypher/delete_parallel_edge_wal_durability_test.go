package cypher_test

// delete_parallel_edge_wal_durability_test.go — engine-level WAL durability
// gate for an instance-precise parallel-edge DELETE (rmp #2018).
//
// Whereas the crash-injection proof (store/recovery) drives the low-level
// txn.Store API, this drives the PUBLIC Cypher engine end-to-end: a
// `MATCH ... DELETE r` on a specifically-bound parallel instance must emit a
// durable OpRemoveEdgeByHandle frame (walMutatorAdapter → Tx), so a pure WAL
// replay across a store reopen leaves exactly that instance gone and the
// sibling intact. It reuses deleteWALEngineRun / deleteWALRecOpts from
// delete_wal_durability_test.go.
//
// Pre-fix: no such op kind existed and the removal targeted the first-match
// slot, so replay would retire T1 and leave T2 — the opposite of the intent.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCypher_DeleteParallelEdgeInstance_WALDurability creates two distinctly
// typed parallel edges through the engine, deletes the T2 instance, then
// recovers from the WAL twice and asserts the surviving edge is T1 only.
func TestCypher_DeleteParallelEdgeInstance_WALDurability(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// open 1: seed nodes and the two parallel typed edges (T1 first, T2 second).
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	deleteWALEngineRun(t, g1, w1,
		`CREATE (a:N {key:'x'})`,
		`CREATE (b:N {key:'y'})`,
		`MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T1]->(b)`,
		`MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T2]->(b)`,
	)

	// open 2 (WAL replay): delete the T2 instance.
	res2, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	deleteWALEngineRun(t, res2.Graph, w2,
		`MATCH (:N {key:'x'})-[r:T2]->(:N {key:'y'}) DELETE r`)

	// open 3 (WAL replay): only the T1 instance may survive.
	res3, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	eng3 := cypher.NewEngine(res3.Graph)

	got := survivingRelTypes(t, eng3)
	if len(got) != 1 || got[0] != "T1" {
		t.Fatalf("after DELETE r:T2 + WAL replay, surviving types = %v, want [T1] (wrong instance durably removed)", got)
	}

	// Belt-and-braces: exactly one adjacency slot survives between the pair.
	srcID, _ := res3.Graph.AdjList().Mapper().Lookup(keyNode(t, res3.Graph, "x"))
	dstID, _ := res3.Graph.AdjList().Mapper().Lookup(keyNode(t, res3.Graph, "y"))
	parallel := 0
	res3.Graph.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src == srcID && tr.Dst == dstID {
			parallel++
		}
		return true
	})
	if parallel != 1 {
		t.Fatalf("recovered %d parallel by-handle instances, want 1", parallel)
	}
}
