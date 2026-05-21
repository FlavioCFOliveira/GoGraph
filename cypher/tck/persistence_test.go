package tck_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"gograph/cypher"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// drainRows drains a *cypher.Result and returns the number of rows consumed.
// It calls t.Fatal on any iteration error and always closes the result.
func drainRows(t *testing.T, res *cypher.Result) int {
	t.Helper()
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("result close: %v", err)
	}
	return n
}

// runCreate executes a single write query via RunInTx and drains the result.
func runCreate(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
	}
	drainRows(t, res)
}

// runMatch executes a read query via Run and returns the row count.
func runMatch(t *testing.T, eng *cypher.Engine, query string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	return drainRows(t, res)
}

// persistAndRecover snapshots g and produces a recovered *cypher.Engine.
//
// Because the Cypher engine writes directly to the LPG (not through any WAL),
// the WAL is empty and recovery.Open would produce an empty graph — it relies
// on WAL replay to intern node identifiers into the mapper before applying
// snapshot labels. To bridge the gap we WAL-log every node/label pair from the
// already-populated LPG using txn.NewStoreWithCodec before snapshotting. The
// re-application of SetNodeLabel ops is idempotent on the in-memory graph.
//
// Flow (mirrors examples/04_persistence/main.go):
//  1. Open a WAL.
//  2. Use txn.Store to WAL-log SetNodeLabel for every (node, label) in g.
//  3. Commit and close the WAL.
//  4. Build CSR and call snapshot.WriteSnapshotFull → csr.bin + labels.bin +
//     properties.bin + manifest.json under <dir>/snapshot.
//  5. Call recovery.Open[string, float64]: WAL replay interns node keys;
//     snapshot labels and properties are then hydrated on top.
func persistAndRecover(t *testing.T, dir string, g *lpg.Graph[string, float64]) *cypher.Engine {
	t.Helper()

	walPath := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snapshot")

	// Step 1: open WAL and log every (node, label) pair so recovery can intern
	// the nodes during WAL replay. SetNodeLabel is idempotent on the LPG.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := txn.NewStoreWithCodec[string, float64](g, w, txn.NewStringCodec())
	tx := store.Begin()
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, nodeKey string) bool {
		for _, label := range g.NodeLabels(nodeKey) {
			if lerr := tx.SetNodeLabel(nodeKey, label); lerr != nil {
				t.Errorf("tx.SetNodeLabel(%q, %q): %v", nodeKey, label, lerr)
			}
		}
		return true
	})
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Step 2: snapshot the full LPG state (CSR + labels.bin + properties.bin).
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Step 3: recover — WAL replay interns nodes, then snapshot labels and
	// properties are applied on top.
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return cypher.NewEngine(res.Graph)
}

// TestTCKPersistence_LabelSurvivesRestart verifies that 50 Person nodes written
// by the Cypher engine survive a WriteSnapshotFull + recovery.Open round-trip
// and are queryable in the recovered engine.
//
// Properties set via Cypher (stored directly in the LPG) are captured by
// properties.bin in the v2 snapshot and therefore also survive the round-trip.
func TestTCKPersistence_LabelSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Create 50 Person nodes with name properties.
	for i := range 50 {
		q := fmt.Sprintf(`CREATE (n:Person {name: "Alice%d"})`, i)
		runCreate(t, eng, q)
	}

	recEng := persistAndRecover(t, dir, g)

	// All 50 Person nodes must be present after recovery.
	got := runMatch(t, recEng, `MATCH (n:Person) RETURN n`)
	if got != 50 {
		t.Errorf("MATCH (n:Person): got %d rows, want 50", got)
	}

	// The first-created node's property must also survive via properties.bin.
	gotProp := runMatch(t, recEng, `MATCH (n:Person {name: "Alice0"}) RETURN n`)
	if gotProp != 1 {
		t.Errorf("MATCH (n:Person {name: 'Alice0'}): got %d rows, want 1 — property round-trip broken", gotProp)
	}
}

// TestTCKPersistence_MultiLabelSurvivesRestart creates 10 nodes each for three
// labels (Person, City, Company) and verifies that each label survives a
// snapshot + recovery round-trip independently.
func TestTCKPersistence_MultiLabelSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	labels := []string{"Person", "City", "Company"}
	for _, label := range labels {
		for i := range 10 {
			q := fmt.Sprintf(`CREATE (n:%s {name: "%s%d"})`, label, label, i)
			runCreate(t, eng, q)
		}
	}

	recEng := persistAndRecover(t, dir, g)

	for _, label := range labels {
		q := fmt.Sprintf(`MATCH (n:%s) RETURN n`, label)
		got := runMatch(t, recEng, q)
		if got != 10 {
			t.Errorf("MATCH (n:%s): got %d rows, want 10", label, got)
		}
	}
}

// TestTCKPersistence_EmptyGraphSurvivesRestart verifies that an empty graph
// snapshots and recovers cleanly, returning zero nodes on MATCH (n) RETURN n.
func TestTCKPersistence_EmptyGraphSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})

	recEng := persistAndRecover(t, dir, g)

	got := runMatch(t, recEng, `MATCH (n) RETURN n`)
	if got != 0 {
		t.Errorf("MATCH (n) on empty recovered graph: got %d rows, want 0", got)
	}
}

// newWALEngine opens a fresh WAL at dir/wal and returns a WAL-backed
// cypher.Engine. The caller owns the returned wal.Writer and must Close it
// when done.
func newWALEngine(t *testing.T, dir string) (*cypher.Engine, *wal.Writer) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store), w
}

// recoverEngineFromWAL opens the same WAL path via recovery.Open and returns a
// plain cypher.Engine backed by the recovered graph.
func recoverEngineFromWAL(t *testing.T, dir string) *cypher.Engine {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return cypher.NewEngine(res.Graph)
}

// TestTCKPersistence_WALDurability_NodeLabel verifies that nodes and labels
// written via RunInTx on a WAL-backed engine survive a close + recovery
// round-trip without any snapshot — pure WAL replay.
func TestTCKPersistence_WALDurability_NodeLabel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	eng, w := newWALEngine(t, dir)

	// Write 10 Person nodes via Cypher.
	for i := range 10 {
		runCreate(t, eng, fmt.Sprintf(`CREATE (n:Person {name: "Bob%d"})`, i))
	}

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recover into a fresh graph from WAL only (no snapshot).
	recEng := recoverEngineFromWAL(t, dir)

	got := runMatch(t, recEng, `MATCH (n:Person) RETURN n`)
	if got != 10 {
		t.Errorf("MATCH (n:Person) after WAL recovery: got %d, want 10", got)
	}
}

// TestTCKPersistence_WALDurability_CrashSimulation simulates a kill -9 by
// writing data via RunInTx, closing the WAL, then recovering into a fresh empty
// graph solely through WAL replay. If the WAL-backed path is wired correctly,
// the freshly recovered graph must contain all written nodes and relationships.
func TestTCKPersistence_WALDurability_CrashSimulation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	eng, w := newWALEngine(t, dir)

	// 5 City nodes, 5 Company nodes.
	for _, lbl := range []string{"City", "Company"} {
		for i := range 5 {
			runCreate(t, eng, fmt.Sprintf(`CREATE (n:%s {name: "%s%d"})`, lbl, lbl, i))
		}
	}
	// 2 Person nodes connected by a KNOWS relationship.
	runCreate(t, eng, `CREATE (a:Person {name: "Alice"})-[:KNOWS]->(b:Person {name: "Charlie"})`)

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Simulate crash + restart: build a completely fresh in-memory graph from
	// the WAL alone — no snapshot, no reference to the old graph.
	recEng := recoverEngineFromWAL(t, dir)

	for _, tc := range []struct {
		label string
		want  int
	}{
		{"City", 5},
		{"Company", 5},
		{"Person", 2},
	} {
		q := fmt.Sprintf(`MATCH (n:%s) RETURN n`, tc.label)
		got := runMatch(t, recEng, q)
		if got != tc.want {
			t.Errorf("MATCH (n:%s) after crash recovery: got %d, want %d", tc.label, got, tc.want)
		}
	}
	// Verify the relationship survived.
	gotRel := runMatch(t, recEng, `MATCH (a:Person {name: "Alice"})-[:KNOWS]->(b) RETURN b`)
	if gotRel != 1 {
		t.Errorf("MATCH relationship after crash recovery: got %d, want 1", gotRel)
	}
}
