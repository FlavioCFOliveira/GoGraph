package cypher_test

// delete_wal_durability_test.go — regression gate for task #1411.
//
// walMutatorAdapter.RemoveNode was missing the a.tx.RemoveNode(n) call that
// emits the OpRemoveNode frame to the WAL. Across a store reopen the deleted
// node resurrected (ghost) because no tombstone frame existed in the WAL
// and no checkpoint had been written.
//
// Pre-fix: the tests below fail because count(n) is 2 after reopen (both
// nodes survive) and MATCH (n:B) returns 1 row.
// Post-fix: they pass because the deleted node remains tombstoned.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// deleteWALRecOpts returns recovery options for the delete durability tests.
func deleteWALRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// deleteWALEngineRun opens a fresh WAL-backed engine over g+w, runs each
// query to full completion (drain + Close), then closes the WAL writer so
// the caller can reopen for recovery. It does NOT checkpoint — this exercises
// the pure WAL replay path so a missing OpRemoveNode frame is immediately
// observable on reopen.
func deleteWALEngineRun(t *testing.T, g *lpg.Graph[string, float64], w *wal.Writer, queries ...string) {
	t.Helper()
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	ctx := context.Background()
	for _, q := range queries {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("RunAny(%q): %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional drain
		}
		if rerr := res.Err(); rerr != nil {
			_ = res.Close()
			t.Fatalf("result error for %q: %v", q, rerr)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("Close(%q): %v", q, cerr)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// TestCypher_DeleteWALDurability_DELETE verifies that a Cypher DELETE
// persists OpRemoveNode to the WAL so the node does not resurrect on reopen.
//
// Sequence:
//
//  1. Open1: CREATE 2 nodes — (:A {name:'keep'}), (:B {name:'gone'})
//  2. Open2 (WAL replay): MATCH (n:B) DELETE n
//  3. Open3 (WAL replay): assert count(n)==1, MATCH (n:B) returns 0 rows.
func TestCypher_DeleteWALDurability_DELETE(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	ctx := context.Background()

	// open 1: create both nodes.
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true})
	deleteWALEngineRun(t, g1, w1,
		`CREATE (:A {name:'keep'}), (:B {name:'gone'})`,
	)

	// open 2 (WAL replay): delete the :B node.
	res2, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	deleteWALEngineRun(t, res2.Graph, w2, `MATCH (n:B) DELETE n`)

	// open 3 (WAL replay): assert the deleted node is gone.
	res3, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	eng3 := cypher.NewEngine(res3.Graph)

	// MATCH (n) RETURN count(n) must equal 1.
	cntRes, err := eng3.RunAny(ctx, `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows := collectRecords(t, cntRes)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	mustInt(t, "count(n) after DELETE", rows[0]["c"], 1)

	// MATCH (n:B) RETURN n must return 0 rows.
	bRes, err := eng3.RunAny(ctx, `MATCH (n:B) RETURN n`, nil)
	if err != nil {
		t.Fatalf("B-label query: %v", err)
	}
	if n := countRows(t, bRes); n != 0 {
		t.Fatalf("MATCH (n:B) returned %d rows after DELETE, want 0 — ghost node resurrected", n)
	}
}

// TestCypher_DeleteWALDurability_DETACHDELETE verifies the same guarantee for
// DETACH DELETE: the node (and any edges) must not reappear on WAL replay.
func TestCypher_DeleteWALDurability_DETACHDELETE(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	ctx := context.Background()

	// open 1: create both nodes and an edge from A to B.
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	deleteWALEngineRun(t, g1, w1,
		`CREATE (a:A {name:'keep'})-[:REL]->(b:B {name:'gone'})`,
	)

	// open 2 (WAL replay): detach delete the :B node.
	res2, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	deleteWALEngineRun(t, res2.Graph, w2, `MATCH (n:B) DETACH DELETE n`)

	// open 3 (WAL replay): assert only :A survives and B is gone.
	res3, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	eng3 := cypher.NewEngine(res3.Graph)

	// MATCH (n) RETURN count(n) must equal 1 (only :A remains).
	cntRes, err := eng3.RunAny(ctx, `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows := collectRecords(t, cntRes)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	mustInt(t, "count(n) after DETACH DELETE", rows[0]["c"], 1)

	// MATCH (n:B) RETURN n must return 0 rows.
	bRes, err := eng3.RunAny(ctx, `MATCH (n:B) RETURN n`, nil)
	if err != nil {
		t.Fatalf("B-label query: %v", err)
	}
	if n := countRows(t, bRes); n != 0 {
		t.Fatalf("MATCH (n:B) returned %d rows after DETACH DELETE, want 0 — ghost node resurrected", n)
	}
}
