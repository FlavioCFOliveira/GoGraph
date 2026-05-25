package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_V3Snapshot_WALAbsent_SelfSufficient is the regression
// guard for the Bug B (rmp task #501) fix. Pre-fix, truncating the
// WAL to zero bytes after a snapshot left recovery.Open reconstructing
// an empty graph because snapshot.WriteSnapshotFull never persisted
// the natural-key mapper. With v3 mapper.bin in place, the snapshot
// alone is enough to rebuild the in-memory graph with every node,
// edge, label and property intact.
//
// The test exercises the canonical string-keyed [string, int64]
// shape that the v3 writer auto-detects via the internal type-switch.
//
//nolint:gocyclo // recovery test: per-element assertions across nodes, edges, labels, properties
func TestRecovery_V3Snapshot_WALAbsent_SelfSufficient(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)

	// Seed: small social graph with edges, node labels, edge labels.
	type commit struct{ src, dst, srcLabel, dstLabel, edgeLabel string }
	commits := []commit{
		{"alice", "bob", "Person", "Person", "KNOWS"},
		{"bob", "carol", "Person", "Person", "KNOWS"},
		{"carol", "dave", "Person", "Person", "FOLLOWS"},
		{"alice", "carol", "Person", "Person", "FOLLOWS"},
	}
	for _, c := range commits {
		tx := store.Begin()
		if err := tx.SetNodeLabel(c.src, c.srcLabel); err != nil {
			t.Fatal(err)
		}
		if err := tx.SetNodeLabel(c.dst, c.dstLabel); err != nil {
			t.Fatal(err)
		}
		if err := tx.AddEdge(c.src, c.dst, 0); err != nil {
			t.Fatal(err)
		}
		if err := tx.SetEdgeLabel(c.src, c.dst, c.edgeLabel); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Typed node + edge properties — these live exclusively in the
	// snapshot's properties.bin (the WAL today does not log typed
	// property writes), so they cover the "mapper restored + props
	// applied" path end-to-end.
	knownTime := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("Alice")); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty("dave", "joined", lpg.TimeValue(knownTime)); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2024")); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	// Snapshot + truncate WAL to 0 bytes. The truncation is exactly
	// what the empirical proof in the bug report uses; it is the
	// strongest possible verification that the snapshot stands on its
	// own without any WAL contribution.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(walPath, 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}

	// Recover from snapshot only.
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false on a v3 snapshot")
	}
	if res.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d",
			res.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 after truncating the WAL", res.WALOps)
	}

	rec := res.Graph
	// Order and Size must match the original graph exactly. Both are
	// exposed via the underlying AdjList (lpg.Graph does not surface
	// them directly).
	if got, want := rec.AdjList().Order(), g.AdjList().Order(); got != want {
		t.Fatalf("Order = %d, want %d", got, want)
	}
	if got, want := rec.AdjList().Size(), g.AdjList().Size(); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
	// Every edge must survive.
	for _, c := range commits {
		if !rec.AdjList().HasEdge(c.src, c.dst) {
			t.Errorf("HasEdge(%q,%q) = false post-recovery", c.src, c.dst)
		}
		if !rec.HasNodeLabel(c.src, c.srcLabel) {
			t.Errorf("HasNodeLabel(%q,%q) = false", c.src, c.srcLabel)
		}
		if !rec.HasNodeLabel(c.dst, c.dstLabel) {
			t.Errorf("HasNodeLabel(%q,%q) = false", c.dst, c.dstLabel)
		}
		if !rec.HasEdgeLabel(c.src, c.dst, c.edgeLabel) {
			t.Errorf("HasEdgeLabel(%q,%q,%q) = false", c.src, c.dst, c.edgeLabel)
		}
	}
	// Properties must survive.
	if v, ok := rec.GetNodeProperty("alice", "name"); !ok {
		t.Error("alice.name missing")
	} else if s, _ := v.String(); s != "Alice" {
		t.Errorf("alice.name = %q, want Alice", s)
	}
	if v, ok := rec.GetNodeProperty("alice", "age"); !ok {
		t.Error("alice.age missing")
	} else if i, _ := v.Int64(); i != 30 {
		t.Errorf("alice.age = %d, want 30", i)
	}
	if v, ok := rec.GetNodeProperty("dave", "joined"); !ok {
		t.Error("dave.joined missing")
	} else if tm, _ := v.Time(); !tm.Equal(knownTime) {
		t.Errorf("dave.joined = %v, want %v", tm, knownTime)
	}
	if v, ok := rec.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Error("edge(alice,bob).since missing")
	} else if s, _ := v.String(); s != "2024" {
		t.Errorf("edge(alice,bob).since = %q, want 2024", s)
	}
}

// TestRecovery_V3Snapshot_RoundTripByteStable confirms that
// write -> load -> write produces identical component files (csr.bin,
// labels.bin, properties.bin, mapper.bin) across the round trip. The
// manifest.json carries CreatedAt so we exclude that timestamp from
// the byte comparison; every other on-disk artifact must be
// bit-identical.
//
// The test exists to pin the determinism property the snapshot writer
// is expected to uphold: the same mapper enumerated in the same order
// must hash to the same CRC and produce the same bytes. A regression
// here typically indicates a non-deterministic iteration entered the
// write path (for example, a map walk in collectMapperPairs that no
// longer respects Walk's shard-major ordering).
func TestRecovery_V3Snapshot_RoundTripByteStable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStore(g, w)
	for _, e := range []struct{ s, d string }{
		{"a", "b"}, {"b", "c"}, {"c", "d"}, {"a", "d"},
	} {
		tx := store.Begin()
		if err := tx.SetNodeLabel(e.s, "L"); err != nil {
			t.Fatal(err)
		}
		if err := tx.AddEdge(e.s, e.d, 0); err != nil {
			t.Fatal(err)
		}
		if err := tx.SetEdgeLabel(e.s, e.d, "E"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.SetNodeProperty("a", "x", lpg.Int64Value(42)); err != nil {
		t.Fatal(err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull (first): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Capture original component bytes.
	originals := captureComponentBytes(t, snapDir)

	// Recover from snapshot alone, then re-snapshot. The recovered
	// graph carries the same interning order in the mapper (LoadFrom
	// preserves it shard-by-shard) and the same adjacency, so the new
	// snapshot's component files must match the originals byte-for-
	// byte.
	if err := os.Truncate(walPath, 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cs2 := csr.BuildFromAdjList(res.Graph.AdjList())
	snapDir2 := filepath.Join(dir, "snapshot2")
	if err := snapshot.WriteSnapshotFull(snapDir2, cs2, res.Graph); err != nil {
		t.Fatalf("WriteSnapshotFull (second): %v", err)
	}
	rewritten := captureComponentBytes(t, snapDir2)

	for _, name := range []string{snapshot.CSRFile, snapshot.LabelsFile,
		snapshot.PropertiesFile, snapshot.MapperFile} {
		if !bytes.Equal(originals[name], rewritten[name]) {
			t.Errorf("component %q drifted across round-trip: %d -> %d bytes",
				name, len(originals[name]), len(rewritten[name]))
		}
	}
}

// captureComponentBytes reads every *.bin file in snapDir and returns
// a map keyed by base name. The manifest.json is intentionally
// excluded because its CreatedAt field forbids bit-identity across
// invocations.
func captureComponentBytes(t *testing.T, snapDir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(snapDir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(entries))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "manifest.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(snapDir, name)) //nolint:gosec // test
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

// TestRecovery_V3Snapshot_WALReplayAfterSnapshot verifies that
// post-snapshot WAL frames are still applied additively on top of the
// restored state. The mapper restore must use Intern semantics so a
// WAL frame referencing a key already seeded by the snapshot returns
// the original NodeID rather than allocating a new slot.
//
// The store is opened via NewStoreWithOptions (v2 frames) so the
// typed Open path replays both pre- and post-snapshot writes; the
// legacy NewStore emits v1 frames that the typed openCodec cannot
// invert and so would not be a valid carrier for this assertion.
func TestRecovery_V3Snapshot_WALReplayAfterSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Pre-snapshot: alice -> bob.
	tx := store.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Post-snapshot: alice -> carol (carol is brand new). The WAL
	// frame replay after the mapper restore must intern carol fresh
	// and AddEdge against the snapshot-restored alice.
	tx2 := store.Begin()
	if err := tx2.AddEdge("alice", "carol", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx2.SetNodeLabel("carol", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := res.Graph
	if !rec.AdjList().HasEdge("alice", "bob") {
		t.Error("alice->bob (from snapshot) missing")
	}
	if !rec.AdjList().HasEdge("alice", "carol") {
		t.Error("alice->carol (from WAL post-snapshot) missing")
	}
	if !rec.HasNodeLabel("alice", "Person") {
		t.Error("alice.Person (from snapshot) missing")
	}
	if !rec.HasNodeLabel("carol", "Person") {
		t.Error("carol.Person (from WAL post-snapshot) missing")
	}
}
