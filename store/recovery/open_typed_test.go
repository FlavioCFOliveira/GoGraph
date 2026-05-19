package recovery

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/index"
	"gograph/graph/index/btree"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestOpen_StringInt64 exercises the canonical [Open] entry point on
// the same `(string, int64)` instantiation that the deprecated
// [OpenString] wrapper has shipped since v1.0.0. The test:
//
//  1. commits a small graph carrying labels and typed properties
//     through a typed Store (NewStoreWithOptions);
//  2. takes a v2 snapshot via [snapshot.WriteSnapshotFull] with one
//     registered btree index;
//  3. reopens the directory via [Open] and asserts every committed
//     surface (edges, labels, properties, index) is rebuilt and that
//     [Result.SnapshotSchemaVersion] reflects the v2 manifest.
//
// The index re-hydration follows the same two-phase pattern as
// [TestRecovery_IndexesSurviveRestart]: Open builds the graph
// without a Manager, then the test wires a fresh Manager onto the
// recovered graph and re-applies the snapshot index payload via the
// package-private [applySnapshotIndexes] helper. This is the
// recommended production startup ordering because [Open] cannot
// know the caller's preferred Manager / Index types in advance.
//
//nolint:gocyclo // test: end-to-end recovery + label + property + index assertions
func TestOpen_StringInt64(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// === Phase 1: write through a typed Store ===
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	bt := btree.New[string]()
	if err := mgr.CreateIndex("btree.score", bt); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", int64(7)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatalf("SetEdgeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	g.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	g.SetEdgeProperty("alice", "bob", "since", lpg.Int64Value(2026))

	// Populate the btree index with a known entry so we can assert
	// it survives a round-trip through the snapshot.
	bt.Insert("score-alice", graph.NodeID(0))

	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// === Phase 2: recover through Open[string, int64] ===
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d (v2)",
			res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("recovered graph missing alice -> bob")
	}
	if got := readEdgeWeightInt64(t, res.Graph, "alice", "bob"); got != 7 {
		t.Fatalf("recovered weight = %d, want 7", got)
	}
	if !res.Graph.HasNodeLabel("alice", "Person") {
		t.Fatal("recovered graph missing alice/Person label")
	}
	if !res.Graph.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("recovered graph missing alice->bob/KNOWS label")
	}
	if v, ok := res.Graph.GetNodeProperty("alice", "name"); !ok {
		t.Fatal("recovered graph missing alice.name")
	} else if s, _ := v.String(); s != "Alice" {
		t.Fatalf("alice.name = %q, want Alice", s)
	}
	if v, ok := res.Graph.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Fatal("recovered graph missing edge property since")
	} else if i, _ := v.Int64(); i != 2026 {
		t.Fatalf("since = %d, want 2026", i)
	}

	// Re-hydrate the btree index onto the freshly recovered graph and
	// verify the entry is back.
	freshMgr := index.NewManager()
	res.Graph.SetIndexManager(freshMgr)
	freshBT := btree.New[string]()
	if err := freshMgr.CreateIndex("btree.score", freshBT); err != nil {
		t.Fatalf("CreateIndex on recovered graph: %v", err)
	}
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if got := applySnapshotIndexes(res.Graph.IndexManager(), loaded.Indexes); got != 1 {
		t.Fatalf("applySnapshotIndexes = %d, want 1", got)
	}
	if !freshBT.Lookup("score-alice").Contains(0) {
		t.Fatal("btree.score index did not survive snapshot round-trip")
	}
}

// TestOpen_Int64Int64 demonstrates Open against an `(int64, int64)`
// instantiation. Recovery has no built-in fallback for non-string N
// types, so this round-trip is only possible through the typed
// codec path — exactly the gap the canonical Open API is meant to
// close.
func TestOpen_Int64Int64(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[int64, int64](g, w, opts)

	commits := []struct {
		src, dst int64
		weight   int64
	}{
		{1, 2, 100},
		{2, 3, 200},
		{3, 4, 300},
	}
	for _, c := range commits {
		tx := s.Begin()
		if err := tx.AddEdge(c.src, c.dst, c.weight); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	res, err := Open[int64, int64](dir, Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.WALOps != len(commits) {
		t.Fatalf("WALOps = %d, want %d", res.WALOps, len(commits))
	}
	for _, c := range commits {
		if !res.Graph.AdjList().HasEdge(c.src, c.dst) {
			t.Errorf("missing edge %d -> %d", c.src, c.dst)
		}
		var got int64
		for n, wgt := range res.Graph.AdjList().Neighbours(c.src) {
			if n == c.dst {
				got = wgt
			}
		}
		if got != c.weight {
			t.Errorf("edge %d->%d weight = %d, want %d", c.src, c.dst, got, c.weight)
		}
	}
}

// TestOpen_Int64Float64 demonstrates Open against an
// `(int64, float64)` instantiation with the canonical
// [txn.NewFloat64WeightCodec]. The test commits a transcendental
// weight whose bit-pattern must round-trip exactly to confirm the
// IEEE-754 encoding is loss-free.
func TestOpen_Int64Float64(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[int64, float64](g, w, opts)

	tx := s.Begin()
	const want = math.Pi
	if err := tx.AddEdge(int64(10), int64(20), want); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	res, err := Open[int64, float64](dir, Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.WALOps != 1 {
		t.Fatalf("WALOps = %d, want 1", res.WALOps)
	}
	if !res.Graph.AdjList().HasEdge(int64(10), int64(20)) {
		t.Fatal("recovered graph missing 10 -> 20")
	}
	var got float64
	for n, wgt := range res.Graph.AdjList().Neighbours(int64(10)) {
		if n == int64(20) {
			got = wgt
		}
	}
	if math.Float64bits(got) != math.Float64bits(want) {
		t.Fatalf("recovered weight = %v (bits 0x%x), want %v (bits 0x%x)",
			got, math.Float64bits(got), want, math.Float64bits(want))
	}
}

// TestOpen_UUIDFloat64 exercises Open against a `([16]byte, float64)`
// instantiation that combines a non-built-in N (the canonical
// [txn.NewUUIDCodec]) with a built-in W. UUIDs are a common
// real-world node identifier, and routing them through the typed
// codec path is the v1.1.1 capstone use-case.
func TestOpen_UUIDFloat64(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[[16]byte, float64](adjlist.Config{Directed: true})
	opts := txn.Options[[16]byte, float64]{
		Codec:       txn.NewUUIDCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[[16]byte, float64](g, w, opts)

	// Two deterministic UUID-shaped keys. The values themselves are
	// irrelevant — what matters is the 16-byte codec round-trip.
	src := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	dst := [16]byte{0xF0, 0xE0, 0xD0, 0xC0, 0xB0, 0xA0, 0x90, 0x80,
		0x70, 0x60, 0x50, 0x40, 0x30, 0x20, 0x10, 0x00}
	const want = 1.4142135623730951 // sqrt(2)

	tx := s.Begin()
	if err := tx.AddEdge(src, dst, want); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	res, err := Open[[16]byte, float64](dir, Options[[16]byte, float64]{
		Codec:       txn.NewUUIDCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.WALOps != 1 {
		t.Fatalf("WALOps = %d, want 1", res.WALOps)
	}
	if !res.Graph.AdjList().HasEdge(src, dst) {
		t.Fatal("recovered graph missing UUID edge")
	}
	var got float64
	for n, wgt := range res.Graph.AdjList().Neighbours(src) {
		if n == dst {
			got = wgt
		}
	}
	if math.Float64bits(got) != math.Float64bits(want) {
		t.Fatalf("recovered weight = %v, want %v", got, want)
	}
}

// TestOpen_SnapshotSchemaVersion_v1 loads the frozen v1 snapshot
// fixture committed at store/snapshot/testdata/v1/sample under the
// canonical [Open] entry point and asserts the new
// [Result.SnapshotSchemaVersion] field reports `1`.
//
// The fixture itself is a snapshot directory only (manifest + csr).
// Recovery expects the snapshot rooted at <dir>/snapshot, so the
// test stages the fixture under a temporary recovery directory by
// copying its files. No WAL is staged: the fixture has no WAL,
// recovery tolerates the absence (snapshot-only restart).
func TestOpen_SnapshotSchemaVersion_v1(t *testing.T) {
	t.Parallel()
	src := filepath.Join("..", "snapshot", "testdata", "v1", "sample")
	tmp := t.TempDir()
	dstSnap := filepath.Join(tmp, "snapshot")
	if err := os.MkdirAll(dstSnap, 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		buf, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // testdata
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dstSnap, e.Name()), buf, 0o600); err != nil { //nolint:gosec // t.TempDir
			t.Fatalf("WriteFile %s: %v", e.Name(), err)
		}
	}

	// The v1 fixture's CSR is keyed by int64 numerically and carries
	// int64 weights. Recovery's WAL replay is a no-op (no WAL), so
	// the codec/weight-codec choice only matters for the (absent)
	// replay path: any compatible pair works.
	res, err := Open[int64, int64](tmp, Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open(v1 fixture): %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (v1 fixture has manifest.json)")
	}
	if res.SnapshotSchemaVersion != 1 {
		t.Fatalf("SnapshotSchemaVersion = %d, want 1 (v1 fixture)",
			res.SnapshotSchemaVersion)
	}
	// v1 manifests carry no labels.bin / properties.bin / indexes.
	if res.SnapshotLabels != 0 {
		t.Errorf("SnapshotLabels = %d, want 0", res.SnapshotLabels)
	}
	if res.SnapshotProperties != 0 {
		t.Errorf("SnapshotProperties = %d, want 0", res.SnapshotProperties)
	}
	if res.SnapshotIndexes != 0 {
		t.Errorf("SnapshotIndexes = %d, want 0", res.SnapshotIndexes)
	}
}

// TestOpenString_DeprecatedButStillWorks confirms the legacy
// [OpenString] wrapper continues to recover a `(string, int64)`
// graph end-to-end after the canonical [Open] API is promoted. The
// new [Result.SnapshotSchemaVersion] field must also be populated
// through the deprecated wrapper because the wrapper shares the
// same snapshot-load code path.
func TestOpenString_DeprecatedButStillWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStore(g, w)
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), cs, g); err != nil {
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
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d",
			res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("OpenString did not recover alice -> bob")
	}
	if !res.Graph.HasNodeLabel("alice", "Person") {
		t.Fatal("OpenString did not recover alice/Person label")
	}
}

// TestOpen_NilCodecRejected mirrors [TestOpenWithOptions_NilCodec]
// for the canonical [Open] entry point.
func TestOpen_NilCodecRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Open[string, int64](dir, Options[string, int64]{
		WeightCodec: txn.NewInt64WeightCodec(),
	}); err == nil {
		t.Fatal("Open with nil codec must error")
	}
	if _, err := Open[string, int64](dir, Options[string, int64]{
		Codec: txn.NewStringCodec(),
	}); err == nil {
		t.Fatal("Open with nil weight codec must error")
	}
}
