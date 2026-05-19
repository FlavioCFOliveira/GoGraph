package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"

	"pgregory.net/rapid"
)

// TestLabels_Roundtrip writes a small fixed LPG to a full v2
// snapshot, reads it back through LoadSnapshotFull, and asserts every
// node and edge label survives the round-trip. The test deliberately
// includes parallel labels per node, edge labels, and a label name
// containing a non-ASCII byte to catch utf-8 length-prefix slips.
func TestLabels_Roundtrip(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("alice", "bob", 1)
	g.AddEdge("bob", "carol", 2)
	g.AddEdge("carol", "alice", 3)
	g.SetNodeLabel("alice", "Person")
	g.SetNodeLabel("alice", "Admin")
	g.SetNodeLabel("bob", "Person")
	g.SetNodeLabel("carol", "Persoa") // intentional unicode-ish glyph
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("bob", "carol", "KNOWS")
	g.SetEdgeLabel("carol", "alice", "FOLLOWS")

	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if loaded.Manifest.Version != ManifestVersion {
		t.Fatalf("Manifest.Version = %d, want %d", loaded.Manifest.Version, ManifestVersion)
	}
	if got := len(loaded.Manifest.Files); got != 2 {
		t.Fatalf("Manifest.Files = %d, want 2 (csr.bin + labels.bin)", got)
	}

	// Materialise the readback into a fresh LPG with the same
	// adjacency replayed: this is the canonical post-restart path,
	// minus a WAL. The fresh graph must therefore have an identical
	// label distribution after ApplyLabelsToGraph.
	restored := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range []struct{ s, d string }{
		{"alice", "bob"}, {"bob", "carol"}, {"carol", "alice"},
	} {
		restored.AddEdge(e.s, e.d, 0)
	}
	if err := ApplyLabelsToGraph(restored, loaded.Labels); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}

	expectedNodeLabels := map[string][]string{
		"alice": {"Admin", "Person"},
		"bob":   {"Person"},
		"carol": {"Persoa"},
	}
	for n, want := range expectedNodeLabels {
		got := restored.NodeLabels(n)
		sort.Strings(got)
		sort.Strings(want)
		if !equalStrings(got, want) {
			t.Errorf("NodeLabels(%q) = %v, want %v", n, got, want)
		}
	}

	expectedEdgeLabels := map[[2]string]string{
		{"alice", "bob"}:   "KNOWS",
		{"bob", "carol"}:   "KNOWS",
		{"carol", "alice"}: "FOLLOWS",
	}
	for k, want := range expectedEdgeLabels {
		if !restored.HasEdgeLabel(k[0], k[1], want) {
			t.Errorf("HasEdgeLabel(%q,%q,%q) = false", k[0], k[1], want)
		}
	}
}

// TestLabels_ManifestV2_LoadsClean confirms a freshly written v2
// snapshot loads back with Version=2 and labels.bin verified
// end-to-end through the manifest CRC.
func TestLabels_ManifestV2_LoadsClean(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("x", "y", 7)
	g.SetNodeLabel("x", "A")
	g.SetEdgeLabel("x", "y", "L")
	c := csr.BuildFromAdjList(g.AdjList())

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if loaded.Manifest.Version != 2 {
		t.Fatalf("Manifest.Version = %d, want 2", loaded.Manifest.Version)
	}
	if len(loaded.Labels.NodeLabels) != 1 {
		t.Fatalf("NodeLabels len = %d, want 1", len(loaded.Labels.NodeLabels))
	}
	if len(loaded.Labels.EdgeLabels) != 1 {
		t.Fatalf("EdgeLabels len = %d, want 1", len(loaded.Labels.EdgeLabels))
	}
}

// TestLabels_ManifestV1_StillLoads verifies the existing v1 testdata
// fixture still loads via LoadSnapshotFull, with an empty
// LabelsReadback because v1 has no labels.bin. The existing
// TestCompat_V1FixtureLoads keeps the [Open] path covered; this
// one specifically pins the v2 helper's forward-compat behaviour.
func TestLabels_ManifestV1_StillLoads(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "v1", "sample")
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull v1 fixture: %v", err)
	}
	if loaded.Manifest.Version != 1 {
		t.Fatalf("Manifest.Version = %d, want 1", loaded.Manifest.Version)
	}
	if loaded.Labels.Strings != nil || loaded.Labels.NodeLabels != nil || loaded.Labels.EdgeLabels != nil {
		t.Fatalf("v1 fixture must yield an empty LabelsReadback, got %+v", loaded.Labels)
	}
}

// TestLabels_CorruptedFile_SurfacesErrCorrupted flips a byte in the
// labels.bin payload and verifies LoadSnapshotFull rejects it with
// the wrapped ErrCorrupted.
func TestLabels_CorruptedFile_SurfacesErrCorrupted(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "L")
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	labelsPath := filepath.Join(dir, LabelsFile)
	data, err := os.ReadFile(labelsPath) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(labelsPath, data, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	_, err = LoadSnapshotFull(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("LoadSnapshotFull(corrupted) = %v, want ErrCorrupted", err)
	}
}

// TestLabels_BadMagic_SurfacesErrLabelsCorrupted feeds a payload with
// a wrong magic header to ReadLabels and verifies the typed error.
func TestLabels_BadMagic_SurfacesErrLabelsCorrupted(t *testing.T) {
	t.Parallel()
	// Eight bytes of zero: wrong magic, wrong version. Magic check
	// fires first, so we exercise the bad-magic branch deterministically.
	buf := bytes.NewReader(make([]byte, 8))
	_, err := ReadLabels(buf)
	if !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("ReadLabels(zero) = %v, want ErrLabelsCorrupted", err)
	}
}

// TestLabels_WriteEmptyGraph_RoundTrips covers the boundary where the
// graph has no labels at all: the writer must still produce a
// structurally valid labels.bin (magic + version + zero counts) and
// the reader must accept it as an empty readback.
func TestLabels_WriteEmptyGraph_RoundTrips(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Labels.Strings) != 0 {
		t.Fatalf("Strings = %d, want 0", len(loaded.Labels.Strings))
	}
	if len(loaded.Labels.NodeLabels) != 0 {
		t.Fatalf("NodeLabels = %d, want 0", len(loaded.Labels.NodeLabels))
	}
	if len(loaded.Labels.EdgeLabels) != 0 {
		t.Fatalf("EdgeLabels = %d, want 0", len(loaded.Labels.EdgeLabels))
	}
}

// TestLabels_PropertyRoundtrip exercises the labels.bin round-trip
// over rapid-generated graphs of up to 16 nodes carrying arbitrary
// label sets per node and per edge. The post-restart label sets
// must equal the originals modulo ordering.
//
//nolint:gocyclo // property test: nested generators + dual-set assertions
func TestLabels_PropertyRoundtrip(t *testing.T) {
	t.Parallel()
	rootTmp := t.TempDir()
	var snapCounter int
	rapid.Check(t, func(t *rapid.T) {
		const labelAlphabet = "PersonAdminKNOWSFOLLOWSEditorReaderMutterer"
		labelGen := rapid.Custom(func(t *rapid.T) string {
			lo := rapid.IntRange(0, len(labelAlphabet)-3).Draw(t, "lo")
			hi := rapid.IntRange(lo+1, len(labelAlphabet)).Draw(t, "hi")
			return labelAlphabet[lo:hi]
		})

		n := rapid.IntRange(1, 16).Draw(t, "n")
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%d", i)
		}

		// Build a random directed graph with random labels.
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		nodeLabels := make(map[string]map[string]bool, n)
		edgeLabels := make(map[[2]string]map[string]bool)
		for _, name := range nodes {
			g.AddNode(name)
			k := rapid.IntRange(0, 4).Draw(t, "node-label-count")
			for i := 0; i < k; i++ {
				l := labelGen.Draw(t, "node-label")
				g.SetNodeLabel(name, l)
				if nodeLabels[name] == nil {
					nodeLabels[name] = make(map[string]bool)
				}
				nodeLabels[name][l] = true
			}
		}
		edgeCount := rapid.IntRange(0, 2*n).Draw(t, "edge-count")
		for i := 0; i < edgeCount; i++ {
			si := rapid.IntRange(0, n-1).Draw(t, "src")
			di := rapid.IntRange(0, n-1).Draw(t, "dst")
			s, d := nodes[si], nodes[di]
			g.AddEdge(s, d, 0)
			k := rapid.IntRange(0, 3).Draw(t, "edge-label-count")
			for j := 0; j < k; j++ {
				l := labelGen.Draw(t, "edge-label")
				g.SetEdgeLabel(s, d, l)
				key := [2]string{s, d}
				if edgeLabels[key] == nil {
					edgeLabels[key] = make(map[string]bool)
				}
				edgeLabels[key][l] = true
			}
		}

		c := csr.BuildFromAdjList(g.AdjList())
		snapCounter++
		dir := filepath.Join(rootTmp, fmt.Sprintf("snap-%d", snapCounter))
		if err := WriteSnapshotFull(dir, c, g); err != nil {
			t.Fatalf("WriteSnapshotFull: %v", err)
		}
		loaded, err := LoadSnapshotFull(dir)
		if err != nil {
			t.Fatalf("LoadSnapshotFull: %v", err)
		}

		// Restore into a fresh graph. The reload contract is: the
		// underlying mapper is populated to the same node IDs the
		// snapshot was written against. We achieve that by adding
		// the same nodes in the same insertion order (n0, n1, ...)
		// and the same edges; the mapper assigns NodeIDs by
		// shard+intra-index, both deterministic given the seed
		// (process-wide maphash seed) and the call sequence.
		//
		// In production the WAL replay performs the equivalent
		// re-emission; here we shortcut via the same nodes/edges
		// slice the test already holds.
		restored := lpg.New[string, int64](adjlist.Config{Directed: true})
		for _, name := range nodes {
			restored.AddNode(name)
		}
		// Re-emit edges in the same order they were added.
		for _, s := range nodes {
			for nb := range g.AdjList().Neighbours(s) {
				restored.AddEdge(s, nb, 0)
			}
		}
		if err := ApplyLabelsToGraph(restored, loaded.Labels); err != nil {
			t.Fatalf("ApplyLabelsToGraph: %v", err)
		}

		// Compare label sets.
		for name, want := range nodeLabels {
			got := restored.NodeLabels(name)
			if !sameSet(got, want) {
				t.Fatalf("NodeLabels(%q) = %v, want set %v", name, got, mapKeys(want))
			}
		}
		for key, want := range edgeLabels {
			got := restored.EdgeLabels(key[0], key[1])
			if !sameSet(got, want) {
				t.Fatalf("EdgeLabels(%q,%q) = %v, want set %v", key[0], key[1], got, mapKeys(want))
			}
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameSet(got []string, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for _, g := range got {
		if !want[g] {
			return false
		}
	}
	return true
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
