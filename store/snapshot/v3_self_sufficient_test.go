package snapshot

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshot_V3SelfSufficient writes a v3 snapshot (string-keyed
// graph, so mapper.bin is emitted) and loads it back via
// [LoadSnapshotFull]. The test asserts that:
//
//  1. Every original node key resolves to a [graph.NodeID] in the
//     loaded mapper pairs (Lookup direction).
//  2. Every [graph.NodeID] resolves back to its original key (Resolve
//     direction).
//
// The "self-sufficient" criterion is that no WAL replay is required
// for the mapper: the snapshot alone carries the full interning table.
// This test operates entirely within the snapshot package and does not
// depend on store/recovery, providing a package-internal regression
// guard.
func TestSnapshot_V3SelfSufficient(t *testing.T) {
	t.Parallel()

	keys := []string{"alice", "bob", "carol", "dave", "eve"}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()

	// Build a 5-node, 5-edge ring so the CSR is non-trivial.
	for i, k := range keys {
		next := keys[(i+1)%len(keys)]
		if err := adj.AddEdge(k, next, int64(i+1)); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", k, next, err)
		}
	}

	snapDir := t.TempDir()
	c := csr.BuildFromAdjList(adj)
	if err := WriteSnapshotFull(snapDir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	loaded, err := LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}

	// Must be a v3 snapshot (mapper.bin emitted for string N).
	if got, want := loaded.Manifest.Version, ManifestVersion; got != want {
		t.Fatalf("Manifest.Version = %d, want %d", got, want)
	}
	if got := len(loaded.Mapper.Pairs); got != len(keys) {
		t.Fatalf("Mapper.Pairs len = %d, want %d", got, len(keys))
	}

	// Build a reverse lookup table from the original mapper (before
	// snapshot) to compare against the loaded pairs.
	origMapper := adj.Mapper()
	byKey := make(map[string]graph.NodeID, len(keys))
	byID := make(map[graph.NodeID]string, len(keys))
	for _, k := range keys {
		id, ok := origMapper.Lookup(k)
		if !ok {
			t.Fatalf("Lookup(%q) not found in original mapper", k)
		}
		byKey[k] = id
		key, ok := origMapper.Resolve(id)
		if !ok {
			t.Fatalf("Resolve(%d) failed in original mapper", id)
		}
		byID[id] = key
	}

	// Assert loaded pairs match the original mapper round-trip.
	for _, p := range loaded.Mapper.Pairs {
		wantKey, ok := byID[p.ID]
		if !ok {
			t.Errorf("loaded pair ID=%d not in original mapper", p.ID)
			continue
		}
		if p.Key != wantKey {
			t.Errorf("loaded pair ID=%d key=%q, want %q", p.ID, p.Key, wantKey)
		}
		wantID := byKey[p.Key]
		if p.ID != wantID {
			t.Errorf("loaded pair key=%q ID=%d, want %d", p.Key, p.ID, wantID)
		}
	}
}
