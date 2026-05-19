package recovery

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_PropertiesSurviveRestart wires the full v2 snapshot
// path into a transactional flow: the test writes a graph with
// typed properties via txn.NewStore (WAL-driven for the edge
// topology) plus direct property sets on the graph, then persists a
// v2 snapshot alongside the WAL via snapshot.WriteSnapshotFull,
// then calls recovery.OpenString to simulate a restart. The
// recovered graph must carry every typed property attached before
// the snapshot.
//
// Note: the WAL today only records label and edge ops — typed
// property writes are kept in memory and flushed exclusively to the
// snapshot's properties.bin. Recovery therefore depends on the
// snapshot apply path producing the same typed value as the
// pre-snapshot in-memory state.
//
//nolint:gocyclo // test: per-property kind assertions across node and edge
func TestRecovery_PropertiesSurviveRestart(t *testing.T) {
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
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("bob", "carol", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Set typed properties directly on the graph. Today these are not
	// WAL-logged; they survive solely via the v2 snapshot
	// properties.bin emission below.
	knownTime := time.Date(2026, 5, 19, 14, 0, 0, 123456789, time.UTC)
	g.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	g.SetNodeProperty("alice", "age", lpg.Int64Value(30))
	g.SetNodeProperty("alice", "score", lpg.Float64Value(99.5))
	g.SetNodeProperty("alice", "active", lpg.BoolValue(true))
	g.SetNodeProperty("alice", "joined", lpg.TimeValue(knownTime))
	g.SetNodeProperty("alice", "blob", lpg.BytesValue([]byte{0x01, 0x02, 0x03}))
	g.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	g.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7))

	// Take a v2 snapshot of the current state.
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
		t.Fatal("SnapshotHit = false")
	}
	if res.SnapshotProperties == 0 {
		t.Fatal("SnapshotProperties = 0, want > 0 after applying v2 properties.bin")
	}

	gRec := res.Graph
	if v, ok := gRec.GetNodeProperty("alice", "name"); !ok {
		t.Fatal("alice.name missing after restart")
	} else if s, _ := v.String(); s != "Alice" {
		t.Fatalf("alice.name = %q, want %q", s, "Alice")
	}
	if v, ok := gRec.GetNodeProperty("alice", "age"); !ok {
		t.Fatal("alice.age missing")
	} else if i, _ := v.Int64(); i != 30 {
		t.Fatalf("alice.age = %d, want 30", i)
	}
	if v, ok := gRec.GetNodeProperty("alice", "score"); !ok {
		t.Fatal("alice.score missing")
	} else if f, _ := v.Float64(); f != 99.5 {
		t.Fatalf("alice.score = %g, want 99.5", f)
	}
	if v, ok := gRec.GetNodeProperty("alice", "active"); !ok {
		t.Fatal("alice.active missing")
	} else if b, _ := v.Bool(); !b {
		t.Fatal("alice.active = false, want true")
	}
	if v, ok := gRec.GetNodeProperty("alice", "joined"); !ok {
		t.Fatal("alice.joined missing")
	} else if tm, _ := v.Time(); !tm.Equal(knownTime) {
		t.Fatalf("alice.joined = %v, want %v", tm, knownTime)
	}
	if v, ok := gRec.GetNodeProperty("alice", "blob"); !ok {
		t.Fatal("alice.blob missing")
	} else if b, _ := v.Bytes(); !bytes.Equal(b, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("alice.blob = %x, want 010203", b)
	}
	if v, ok := gRec.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Fatal("edge(alice,bob).since missing")
	} else if s, _ := v.String(); s != "2026" {
		t.Fatalf("edge(alice,bob).since = %q, want 2026", s)
	}
	if v, ok := gRec.GetEdgeProperty("alice", "bob", "weight"); !ok {
		t.Fatal("edge(alice,bob).weight missing")
	} else if i, _ := v.Int64(); i != 7 {
		t.Fatalf("edge(alice,bob).weight = %d, want 7", i)
	}
}

// TestRecovery_V1SnapshotPropertiesEmpty asserts that an old v1
// snapshot (no properties.bin) coexisting with the WAL continues to
// load cleanly via OpenString. SnapshotHit is true and
// SnapshotProperties is 0 — the forward-compat contract.
func TestRecovery_V1SnapshotPropertiesEmpty(t *testing.T) {
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
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
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
		t.Fatal("SnapshotHit = false")
	}
	if res.SnapshotProperties != 0 {
		t.Fatalf("SnapshotProperties = %d, want 0 (v1 snapshot has no properties.bin)",
			res.SnapshotProperties)
	}
}
