package snapshot

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
)

// TestSnapshot_ForwardCompat writes a v3 snapshot via
// [WriteSnapshotFull] (string-keyed graph so mapper.bin is emitted)
// and asserts that [LoadSnapshotFull] accepts it and returns
// [Manifest.Version] == [ManifestVersion] (currently 3). The test
// closes the acceptance criterion that the current loader accepts
// the current writer's output and that the manifest version field is
// faithfully propagated to callers.
//
// Coverage note: [TestCompat_V1FixtureLoads] already pins the v1
// backward-compatibility path; this test specifically exercises the
// forward direction — that the highest-version snapshot the build
// can write is also the highest version it can read.
func TestSnapshot_ForwardCompat(t *testing.T) {
	t.Parallel()
	// Build a small string-keyed graph so WriteSnapshotFull produces
	// a v3 manifest (mapper.bin emitted for string N).
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AdjList().AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AdjList().AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("a", "Start"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("b", "weight", lpg.Int64Value(42)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	snapDir := t.TempDir()
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(snapDir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	loaded, err := LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}

	// A string-keyed graph produces a v3 manifest (mapper.bin
	// included).
	if got, want := loaded.Manifest.Version, ManifestVersion; got != want {
		t.Fatalf("Manifest.Version = %d, want %d (ManifestVersion)", got, want)
	}

	// The CSR must carry two edges.
	if got := len(loaded.CSR.Edges); got != 2 {
		t.Fatalf("loaded CSR edges = %d, want 2", got)
	}

	// Labels section must carry the one label we wrote.
	if len(loaded.Labels.NodeLabels) == 0 {
		t.Fatalf("loaded labels: want at least one node-label record")
	}

	// Mapper must carry all three interned keys.
	if got := len(loaded.Mapper.Pairs); got != 3 {
		t.Fatalf("loaded mapper pairs = %d, want 3", got)
	}
}
