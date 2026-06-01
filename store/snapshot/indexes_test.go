package snapshot

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seededFixture bundles the graph, the manager, and the three
// registered indexes used across the multi-index tests in this file.
type seededFixture struct {
	g   *lpg.Graph[string, int64]
	mgr *index.Manager
	lab *label.Index
	hsh *hash.Index[string]
	bt  *btree.Index[string]
}

// seedThreeIndexes builds a fresh lpg.Graph with three registered
// secondary indexes (label / hash / btree), populates each with a
// known set of (key, NodeID) tuples, and returns them in a
// [seededFixture]. Used by every multi-index test in this file.
func seedThreeIndexes(t *testing.T) seededFixture {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)

	lab := label.NewIndex()
	hsh := hash.New[string]()
	bt := btree.New[string]()

	if err := mgr.CreateIndex("labels.nodes", lab); err != nil {
		t.Fatalf("CreateIndex labels.nodes: %v", err)
	}
	if err := mgr.CreateIndex("hash.email", hsh); err != nil {
		t.Fatalf("CreateIndex hash.email: %v", err)
	}
	if err := mgr.CreateIndex("btree.age", bt); err != nil {
		t.Fatalf("CreateIndex btree.age: %v", err)
	}

	// Add a handful of nodes so the underlying CSR is non-trivial.
	for i := 0; i < 16; i++ {
		if err := g.AddNode(string(rune('a' + i))); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := uint64(0); i < 16; i++ {
		lab.Add(1, graph.NodeID(i))
		if i%2 == 0 {
			lab.Add(2, graph.NodeID(i))
		}
		hsh.Insert("user-"+string(rune('a'+i)), graph.NodeID(i))
		bt.Insert("k"+string(rune('a'+i)), graph.NodeID(i))
	}
	return seededFixture{g: g, mgr: mgr, lab: lab, hsh: hsh, bt: bt}
}

// TestSnapshot_IndexesPersisted writes a v2 snapshot with three
// registered indexes, reloads it via LoadSnapshotFull, and asserts
// every readback's bytes are non-nil (CRC validated). The acceptance
// criterion: round-trip preserves indexes through the snapshot
// directory.
func TestSnapshot_IndexesPersisted(t *testing.T) {
	t.Parallel()
	fx := seedThreeIndexes(t)
	g := fx.g
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Indexes sub-directory must contain three files.
	entries, err := os.ReadDir(filepath.Join(dir, IndexesDir))
	if err != nil {
		t.Fatalf("ReadDir(indexes): %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("indexes/ contains %d files, want 3", len(entries))
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Indexes) != 3 {
		t.Fatalf("loaded.Indexes len = %d, want 3", len(loaded.Indexes))
	}
	for _, rb := range loaded.Indexes {
		if rb.Bytes == nil {
			t.Fatalf("index %q readback returned nil bytes (corruption detected unexpectedly)", rb.Name)
		}
	}
	// Manifest carries the index sub-list.
	if len(loaded.Manifest.Indexes) != 3 {
		t.Fatalf("Manifest.Indexes len = %d, want 3", len(loaded.Manifest.Indexes))
	}
}

// TestSnapshot_NoManagerNoIndexes confirms WriteSnapshotFull on a
// graph with no IndexManager attached produces no indexes/ directory
// and no manifest entry — preserving byte-identical output for
// callers that never opt in.
func TestSnapshot_NoManagerNoIndexes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexesDir)); !os.IsNotExist(err) {
		t.Fatalf("indexes/ dir = %v, want IsNotExist", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Manifest.Indexes) != 0 {
		t.Fatalf("Manifest.Indexes = %v, want empty for graph with no manager", loaded.Manifest.Indexes)
	}
	if loaded.Indexes != nil {
		t.Fatalf("Indexes readback = %v, want nil for empty manifest", loaded.Indexes)
	}
}

// TestSnapshot_EmptyManagerNoIndexes confirms a manager with zero
// registered indexes does NOT emit the indexes/ directory.
func TestSnapshot_EmptyManagerNoIndexes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.SetIndexManager(index.NewManager())
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexesDir)); !os.IsNotExist(err) {
		t.Fatalf("indexes/ dir present when manager is empty, err=%v", err)
	}
}

// TestSnapshot_CorruptedIndex_Tolerated writes a snapshot with three
// registered indexes, flips a byte in the second index file's
// trailer, and asserts that LoadSnapshotFull still succeeds and that
// the corrupted index surfaces as a nil-Bytes IndexReadback. The
// other two indexes survive intact.
func TestSnapshot_CorruptedIndex_Tolerated(t *testing.T) {
	t.Parallel()
	fx := seedThreeIndexes(t)
	g := fx.g
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Pick one index file and corrupt its trailing CRC32C.
	files, err := os.ReadDir(filepath.Join(dir, IndexesDir))
	if err != nil {
		t.Fatalf("ReadDir indexes: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no index files written")
	}
	target := filepath.Join(dir, IndexesDir, files[1].Name())
	buf, err := os.ReadFile(target) //nolint:gosec // testdata path under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	buf[len(buf)-1] ^= 0xFF
	if err := os.WriteFile(target, buf, 0o600); err != nil { //nolint:gosec // testdata path under t.TempDir()
		t.Fatal(err)
	}

	// LoadSnapshotFull must still succeed.
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull on corrupted index = %v, want nil", err)
	}
	// One readback's Bytes are nil.
	nilCount := 0
	okCount := 0
	for _, rb := range loaded.Indexes {
		if rb.Bytes == nil {
			nilCount++
		} else {
			okCount++
		}
	}
	if nilCount != 1 {
		t.Fatalf("nil Bytes count = %d, want 1", nilCount)
	}
	if okCount != 2 {
		t.Fatalf("ok readback count = %d, want 2", okCount)
	}
}

// TestSnapshot_MissingIndexDir_Tolerated removes the indexes/
// directory after writing the snapshot and asserts LoadSnapshotFull
// still succeeds, reporting every index as nil-Bytes (rebuild path).
func TestSnapshot_MissingIndexDir_Tolerated(t *testing.T) {
	t.Parallel()
	fx := seedThreeIndexes(t)
	g := fx.g
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, IndexesDir)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull after removing indexes/ = %v, want nil", err)
	}
	for _, rb := range loaded.Indexes {
		if rb.Bytes != nil {
			t.Fatalf("readback %q Bytes != nil after dir removed", rb.Name)
		}
	}
}

// TestSnapshot_V1FixtureStillLoads guards rmp #172's backward-compat
// constraint: v1 snapshots in testdata/v1/ must keep loading cleanly
// with an empty Indexes readback list.
func TestSnapshot_V1FixtureStillLoads(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "v1", "sample")
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull(v1 fixture) = %v, want nil", err)
	}
	if loaded.Manifest.Version != 1 {
		t.Fatalf("Manifest.Version = %d, want 1", loaded.Manifest.Version)
	}
	if len(loaded.Manifest.Indexes) != 0 {
		t.Fatalf("v1 fixture Manifest.Indexes = %v, want empty",
			loaded.Manifest.Indexes)
	}
	if len(loaded.Indexes) != 0 {
		t.Fatalf("v1 fixture Indexes readback = %v, want empty", loaded.Indexes)
	}
}

// TestSnapshot_NonSerializableSubscriberSkipped registers a
// subscriber that does not implement [index.Serializer] and asserts
// the snapshot writer silently skips it — the manifest must not
// reference it, and no file is written.
func TestSnapshot_NonSerializableSubscriberSkipped(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	if err := mgr.CreateIndex("nope", &dummySub{}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Manifest.Indexes) != 0 {
		t.Fatalf("non-serializable subscriber recorded in manifest: %v",
			loaded.Manifest.Indexes)
	}
}

type dummySub struct{}

func (*dummySub) Apply(index.Change) {}
func (*dummySub) Kind() string       { return "dummy" }

// TestSnapshot_ManifestCRCIntegrity confirms the per-index manifest
// CRC32C matches a fresh checksum of the on-disk bytes — useful as a
// sanity check whenever the writer is touched.
func TestSnapshot_ManifestCRCIntegrity(t *testing.T) {
	t.Parallel()
	fx := seedThreeIndexes(t)
	g := fx.g
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	for _, e := range loaded.Manifest.Indexes {
		path := filepath.Join(dir, IndexesDir, e.Name+".bin")
		buf, err := os.ReadFile(path) //nolint:gosec // path built from manifest
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		if int64(len(buf)) != e.Size {
			t.Fatalf("size mismatch %s: file=%d manifest=%d", path, len(buf), e.Size)
		}
		got := crc32.Checksum(buf, castagnoli)
		if got != e.CRC32C {
			t.Fatalf("crc mismatch %s: got=%d manifest=%d", path, got, e.CRC32C)
		}
	}
}
