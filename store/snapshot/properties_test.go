package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"

	"pgregory.net/rapid"
)

// TestProperties_RoundtripAllKinds writes one property of each of the
// six PropertyValue kinds on a small graph, round-trips it through
// WriteSnapshotFull + LoadSnapshotFull, and asserts every typed
// value survives bit-for-bit. It deliberately picks values whose
// encoding stresses the format:
//
//   - PropString: contains a multi-byte rune so utf-8 length prefixes
//     have to match byte length (not rune count).
//   - PropInt64: negative value so two's-complement encoding is exercised.
//   - PropFloat64: a non-trivial double whose mantissa is not zero.
//   - PropBool: both true and false on different properties.
//   - PropTime: a known UTC instant with nanosecond precision.
//   - PropBytes: a non-utf8 byte sequence.
//
//nolint:gocyclo // round-trip test: one property per kind on node and edge
func TestProperties_RoundtripAllKinds(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("bob", "carol", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// Node-side property fixture: one of each kind.
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("Álice")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(-42)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "score", lpg.Float64Value(3.141592653589793)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "active", lpg.BoolValue(true)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "inactive", lpg.BoolValue(false)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	tStamp := time.Date(2026, 5, 19, 12, 34, 56, 789012345, time.UTC)
	if err := g.SetNodeProperty("alice", "joined", lpg.TimeValue(tStamp)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("alice", "blob", lpg.BytesValue([]byte{0x00, 0xFF, 0xAB, 0xCD, 0xEF})); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	// Edge-side property fixture: one of each kind on a different edge
	// to confirm src/dst tagging is correct.
	for _, ep := range []struct {
		key string
		val lpg.PropertyValue
	}{
		{"since", lpg.StringValue("2026")},
		{"weight", lpg.Int64Value(7)},
		{"score", lpg.Float64Value(-0.5)},
		{"active", lpg.BoolValue(true)},
		{"first_seen", lpg.TimeValue(tStamp)},
		{"raw", lpg.BytesValue([]byte("\x00\x01\x02"))},
	} {
		if err := g.SetEdgeProperty("alice", "bob", ep.key, ep.val); err != nil {
			t.Fatalf("SetEdgeProperty %q: %v", ep.key, err)
		}
	}

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
	if len(loaded.Properties.NodeProperties) != 7 {
		t.Fatalf("NodeProperties entries = %d, want 7",
			len(loaded.Properties.NodeProperties))
	}
	if len(loaded.Properties.EdgeProperties) != 6 {
		t.Fatalf("EdgeProperties entries = %d, want 6",
			len(loaded.Properties.EdgeProperties))
	}

	// Materialise into a fresh graph with the same adjacency replayed,
	// then apply the properties readback.
	restored := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := restored.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := restored.AddEdge("bob", "carol", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := ApplyPropertiesToGraph(restored, loaded.Properties); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}

	// Node-side assertions.
	mustGetNodeString(t, restored, "alice", "name", "Álice")
	mustGetNodeInt64(t, restored, "alice", "age", -42)
	mustGetNodeFloat64(t, restored, "alice", "score", 3.141592653589793)
	mustGetNodeBool(t, restored, "alice", "active", true)
	mustGetNodeBool(t, restored, "alice", "inactive", false)
	mustGetNodeTime(t, restored, "alice", "joined", tStamp)
	mustGetNodeBytes(t, restored, "alice", "blob", []byte{0x00, 0xFF, 0xAB, 0xCD, 0xEF})

	// Edge-side assertions.
	mustGetEdgeString(t, restored, "alice", "bob", "since", "2026")
	mustGetEdgeInt64(t, restored, "alice", "bob", "weight", 7)
	mustGetEdgeFloat64(t, restored, "alice", "bob", "score", -0.5)
	mustGetEdgeBool(t, restored, "alice", "bob", "active", true)
	mustGetEdgeTime(t, restored, "alice", "bob", "first_seen", tStamp)
	mustGetEdgeBytes(t, restored, "alice", "bob", "raw", []byte("\x00\x01\x02"))
}

// TestProperties_ManifestCurrent_WithBothLabelsAndProperties_Loads
// confirms a manifest carrying both labels.bin and properties.bin
// loads back through the current writer, with both readbacks
// populated. String-keyed graphs produce a v3 manifest (with
// mapper.bin) in current builds.
func TestProperties_ManifestCurrent_WithBothLabelsAndProperties_Loads(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	g.SetEdgeLabel("a", "b", "E")
	if err := g.SetNodeProperty("a", "k", lpg.Int64Value(99)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "k", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

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
	if len(loaded.Labels.NodeLabels) != 1 || len(loaded.Labels.EdgeLabels) != 1 {
		t.Fatalf("labels readback wrong: %+v", loaded.Labels)
	}
	if len(loaded.Properties.NodeProperties) != 1 || len(loaded.Properties.EdgeProperties) != 1 {
		t.Fatalf("properties readback wrong: %+v", loaded.Properties)
	}
}

// TestProperties_ManifestV2_OnlyLabels_Loads stages a v2 manifest
// that explicitly omits the properties.bin entry. The reader must
// load cleanly and return an empty PropertiesReadback.
func TestProperties_ManifestV2_OnlyLabels_Loads(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	// Re-write a v2 manifest by hand, omitting the properties.bin
	// entry: this simulates a v2 snapshot taken by an older build
	// that only knew about labels.bin.
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	filtered := make([]FileEntry, 0, len(m.Files))
	for _, f := range m.Files {
		if f.Name == PropertiesFile {
			continue
		}
		filtered = append(filtered, f)
	}
	m.Files = filtered
	mf, err := os.Create(manifestPath) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()
		t.Fatal(err)
	}
	_ = mf.Close()
	if err := os.Remove(filepath.Join(dir, PropertiesFile)); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Labels.NodeLabels) != 1 {
		t.Fatalf("labels missing after partial v2 load: %+v", loaded.Labels)
	}
	if len(loaded.Properties.NodeProperties) != 0 || len(loaded.Properties.EdgeProperties) != 0 {
		t.Fatalf("properties should be empty when properties.bin absent: %+v", loaded.Properties)
	}
}

// TestProperties_ManifestV2_NoLabelsNoProperties_Loads stages a v2
// manifest with neither labels.bin nor properties.bin. The reader
// must load cleanly and return empty readbacks for both.
//
//nolint:gocyclo // staging path: rewrite manifest + delete two files + dual readback assertions
func TestProperties_ManifestV2_NoLabelsNoProperties_Loads(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	// Strip both labels.bin and properties.bin from the manifest +
	// disk, leaving a CSR-only v2 directory.
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	filtered := make([]FileEntry, 0, len(m.Files))
	for _, f := range m.Files {
		if f.Name == LabelsFile || f.Name == PropertiesFile {
			continue
		}
		filtered = append(filtered, f)
	}
	m.Files = filtered
	mf, err := os.Create(manifestPath) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()
		t.Fatal(err)
	}
	_ = mf.Close()
	if err := os.Remove(filepath.Join(dir, LabelsFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, PropertiesFile)); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if loaded.Labels.Strings != nil || loaded.Labels.NodeLabels != nil || loaded.Labels.EdgeLabels != nil {
		t.Fatalf("labels readback should be empty: %+v", loaded.Labels)
	}
	if loaded.Properties.Keys != nil || loaded.Properties.NodeProperties != nil || loaded.Properties.EdgeProperties != nil {
		t.Fatalf("properties readback should be empty: %+v", loaded.Properties)
	}
}

// TestProperties_ManifestV1_StillLoads verifies that the frozen v1
// fixture still loads via the v2 helper; the returned
// PropertiesReadback is the zero value because v1 has no
// properties.bin.
func TestProperties_ManifestV1_StillLoads(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "v1", "sample")
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull v1 fixture: %v", err)
	}
	if loaded.Manifest.Version != 1 {
		t.Fatalf("Manifest.Version = %d, want 1", loaded.Manifest.Version)
	}
	if loaded.Properties.Keys != nil ||
		loaded.Properties.NodeProperties != nil ||
		loaded.Properties.EdgeProperties != nil {
		t.Fatalf("v1 fixture must yield an empty PropertiesReadback, got %+v", loaded.Properties)
	}
}

// TestProperties_WriteEmptyGraph_RoundTrips covers the boundary
// where the graph carries no properties: the writer must still
// emit a structurally valid file and the reader must accept it as
// an empty readback.
func TestProperties_WriteEmptyGraph_RoundTrips(t *testing.T) {
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
	if len(loaded.Properties.Keys) != 0 {
		t.Fatalf("Keys = %d, want 0", len(loaded.Properties.Keys))
	}
	if len(loaded.Properties.NodeProperties) != 0 {
		t.Fatalf("NodeProperties = %d, want 0", len(loaded.Properties.NodeProperties))
	}
	if len(loaded.Properties.EdgeProperties) != 0 {
		t.Fatalf("EdgeProperties = %d, want 0", len(loaded.Properties.EdgeProperties))
	}
}

// TestProperties_CorruptedFile_SurfacesErrCorrupted flips a byte in
// the properties.bin payload and verifies LoadSnapshotFull rejects
// it with the wrapped ErrCorrupted.
func TestProperties_CorruptedFile_SurfacesErrCorrupted(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeProperty("a", "k", lpg.Int64Value(42)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PropertiesFile)
	data, err := os.ReadFile(path) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	_, err = LoadSnapshotFull(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("LoadSnapshotFull(corrupted) = %v, want ErrCorrupted", err)
	}
}

// TestProperties_BadMagic_SurfacesErrPropertiesCorrupted feeds a
// payload with a wrong magic header to ReadProperties and verifies
// the typed error.
func TestProperties_BadMagic_SurfacesErrPropertiesCorrupted(t *testing.T) {
	t.Parallel()
	// Eight bytes of zero: wrong magic, wrong version. Magic check
	// fires first, so we exercise the bad-magic branch deterministically.
	buf := bytes.NewReader(make([]byte, 8))
	_, err := ReadProperties(buf)
	if !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("ReadProperties(zero) = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestProperties_PropertyRoundtrip drives a rapid-generated graph
// through the full snapshot round-trip with random typed properties
// across all six kinds. Properties are sampled from the full kind
// set so the assertion grid exercises decoding for every kind.
//
//nolint:gocyclo // property test: nested generators + per-kind value comparison
func TestProperties_PropertyRoundtrip(t *testing.T) {
	t.Parallel()
	rootTmp := t.TempDir()
	var snapCounter int
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(t, "n")
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%d", i)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})

		// Expected state. Property values stored as their canonical
		// post-round-trip representation so the comparator does not
		// need to special-case time / float / bytes again.
		type nodeKey struct{ node, key string }
		type edgeKey struct{ src, dst, key string }
		expectedNode := make(map[nodeKey]lpg.PropertyValue)
		expectedEdge := make(map[edgeKey]lpg.PropertyValue)

		// Build random typed properties on each node.
		for _, name := range nodes {
			if err := g.AddNode(name); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			k := rapid.IntRange(0, 4).Draw(t, "node-prop-count")
			for i := 0; i < k; i++ {
				key := fmt.Sprintf("k%d", rapid.IntRange(0, 5).Draw(t, "key-id"))
				v := drawRandomPropertyValue(t)
				if err := g.SetNodeProperty(name, key, v); err != nil {
					t.Fatalf("SetNodeProperty: %v", err)
				}
				expectedNode[nodeKey{name, key}] = canonicaliseValue(v)
			}
		}
		// Random edges.
		edgeCount := rapid.IntRange(0, 2*n).Draw(t, "edge-count")
		for i := 0; i < edgeCount; i++ {
			si := rapid.IntRange(0, n-1).Draw(t, "src")
			di := rapid.IntRange(0, n-1).Draw(t, "dst")
			s, d := nodes[si], nodes[di]
			if err := g.AddEdge(s, d, 0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			k := rapid.IntRange(0, 3).Draw(t, "edge-prop-count")
			for j := 0; j < k; j++ {
				key := fmt.Sprintf("ek%d", rapid.IntRange(0, 4).Draw(t, "edge-key-id"))
				v := drawRandomPropertyValue(t)
				if err := g.SetEdgeProperty(s, d, key, v); err != nil {
					t.Fatalf("SetEdgeProperty: %v", err)
				}
				expectedEdge[edgeKey{s, d, key}] = canonicaliseValue(v)
			}
		}

		// Round-trip.
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

		// Replay against a fresh graph with the same adjacency.
		restored := lpg.New[string, int64](adjlist.Config{Directed: true})
		for _, name := range nodes {
			if err := restored.AddNode(name); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for _, s := range nodes {
			for nb := range g.AdjList().Neighbours(s) {
				if err := restored.AddEdge(s, nb, 0); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
		}
		if err := ApplyPropertiesToGraph(restored, loaded.Properties); err != nil {
			t.Fatalf("ApplyPropertiesToGraph: %v", err)
		}

		for k, want := range expectedNode {
			got, ok := restored.GetNodeProperty(k.node, k.key)
			if !ok {
				t.Fatalf("missing node property %v", k)
			}
			if !propertyValuesEqual(got, want) {
				t.Fatalf("node property %v: got %v, want %v",
					k, debugProp(got), debugProp(want))
			}
		}
		for k, want := range expectedEdge {
			got, ok := restored.GetEdgeProperty(k.src, k.dst, k.key)
			if !ok {
				t.Fatalf("missing edge property %v", k)
			}
			if !propertyValuesEqual(got, want) {
				t.Fatalf("edge property %v: got %v, want %v",
					k, debugProp(got), debugProp(want))
			}
		}
	})
}

// drawRandomPropertyValue samples one of the six kinds uniformly.
func drawRandomPropertyValue(t *rapid.T) lpg.PropertyValue {
	kind := rapid.IntRange(1, 6).Draw(t, "kind")
	switch lpg.PropertyKind(kind) {
	case lpg.PropString:
		// Bound the string size so rapid does not generate megabyte
		// payloads on every iteration.
		s := rapid.StringN(0, 32, -1).Draw(t, "str")
		return lpg.StringValue(s)
	case lpg.PropInt64:
		return lpg.Int64Value(rapid.Int64().Draw(t, "i64"))
	case lpg.PropFloat64:
		// Avoid NaN values: NaN != NaN by IEEE rules and we compare
		// floats with == in the assertion. Bits-comparison is overkill
		// for the round-trip contract here (Float64 already serialises
		// math.Float64bits round-trip; that is exercised in the
		// dedicated TestProperties_RoundtripAllKinds).
		f := rapid.Float64().Filter(func(f float64) bool { return !math.IsNaN(f) }).Draw(t, "f64")
		return lpg.Float64Value(f)
	case lpg.PropBool:
		return lpg.BoolValue(rapid.Bool().Draw(t, "b"))
	case lpg.PropTime:
		// Sample seconds-since-epoch in a reasonable window so the
		// time arithmetic stays inside time.Time's representable
		// range. nsec is independent so the nanosecond component is
		// exercised.
		sec := rapid.Int64Range(0, 4_000_000_000).Draw(t, "sec")
		nsec := rapid.Int64Range(0, 999_999_999).Draw(t, "nsec")
		return lpg.TimeValue(time.Unix(sec, nsec).UTC())
	case lpg.PropBytes:
		b := rapid.SliceOfN(rapid.Byte(), 0, 32).Draw(t, "bytes")
		return lpg.BytesValue(b)
	}
	t.Fatalf("unreachable: drew kind=%d", kind)
	return lpg.PropertyValue{}
}

// canonicaliseValue normalises a PropertyValue to the form it takes
// after a write/read cycle. For PropTime that means stripping the
// monotonic clock reading and forcing UTC. Other kinds are
// pass-through.
func canonicaliseValue(v lpg.PropertyValue) lpg.PropertyValue {
	if t, ok := v.Time(); ok {
		// Forcing through time.Unix(t.Unix(), t.Nanosecond()) drops
		// the monotonic component and matches the reader's UTC
		// reconstruction.
		return lpg.TimeValue(time.Unix(t.Unix(), int64(t.Nanosecond())).UTC())
	}
	return v
}

// propertyValuesEqual compares two PropertyValues by kind + typed
// payload. Bytes equality uses bytes.Equal; everything else uses the
// natural == on the underlying typed value.
func propertyValuesEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		as, _ := a.String()
		bs, _ := b.String()
		return as == bs
	case lpg.PropInt64:
		ai, _ := a.Int64()
		bi, _ := b.Int64()
		return ai == bi
	case lpg.PropFloat64:
		af, _ := a.Float64()
		bf, _ := b.Float64()
		return math.Float64bits(af) == math.Float64bits(bf)
	case lpg.PropBool:
		ab, _ := a.Bool()
		bb, _ := b.Bool()
		return ab == bb
	case lpg.PropTime:
		at, _ := a.Time()
		bt, _ := b.Time()
		return at.Equal(bt)
	case lpg.PropBytes:
		ab, _ := a.Bytes()
		bb, _ := b.Bytes()
		return bytes.Equal(ab, bb)
	}
	return false
}

// debugProp returns a readable representation of v for test failure
// messages.
func debugProp(v lpg.PropertyValue) string {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return fmt.Sprintf("String(%q)", s)
	case lpg.PropInt64:
		i, _ := v.Int64()
		return fmt.Sprintf("Int64(%d)", i)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return fmt.Sprintf("Float64(%g, bits=%#016x)", f, math.Float64bits(f))
	case lpg.PropBool:
		b, _ := v.Bool()
		return fmt.Sprintf("Bool(%v)", b)
	case lpg.PropTime:
		t, _ := v.Time()
		return fmt.Sprintf("Time(%s)", t.Format(time.RFC3339Nano))
	case lpg.PropBytes:
		b, _ := v.Bytes()
		return fmt.Sprintf("Bytes(%x)", b)
	}
	return "PropertyValue{?}"
}

// TestProperties_KeyOrderIsStableAcrossSnapshots asserts the on-disk
// key table preserves interning order across writer invocations.
// This is a documentation lock: future readers may rely on the
// invariant.
func TestProperties_KeyOrderIsStableAcrossSnapshots(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeProperty("n", "zebra", lpg.Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("n", "apple", lpg.Int64Value(2)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("n", "monkey", lpg.Int64Value(3)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"zebra", "apple", "monkey"} // interning order
	if len(loaded.Properties.Keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", loaded.Properties.Keys, want)
	}
	for i := range want {
		if loaded.Properties.Keys[i] != want[i] {
			t.Fatalf("Keys[%d] = %q, want %q (full=%v)",
				i, loaded.Properties.Keys[i], want[i], loaded.Properties.Keys)
		}
	}
}

// TestProperties_MultipleNodesEdges asserts that the writer correctly
// emits one record per (node, key, value) tuple even when many
// nodes and edges contribute properties to the same key.
func TestProperties_MultipleNodesEdges(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	const N = 50
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("n%d", i)
		if err := g.AddNode(name); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(name, "idx", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < N-1; i++ {
		s, d := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1)
		if err := g.AddEdge(s, d, 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := g.SetEdgeProperty(s, d, "ord", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetEdgeProperty: %v", err)
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Properties.NodeProperties) != N {
		t.Fatalf("NodeProperties = %d, want %d", len(loaded.Properties.NodeProperties), N)
	}
	if len(loaded.Properties.EdgeProperties) != N-1 {
		t.Fatalf("EdgeProperties = %d, want %d", len(loaded.Properties.EdgeProperties), N-1)
	}

	restored := lpg.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < N; i++ {
		if err := restored.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < N-1; i++ {
		if err := restored.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	if err := ApplyPropertiesToGraph(restored, loaded.Properties); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		v, ok := restored.GetNodeProperty(fmt.Sprintf("n%d", i), "idx")
		if !ok {
			t.Fatalf("missing idx on n%d", i)
		}
		if gi, _ := v.Int64(); gi != int64(i) {
			t.Fatalf("n%d idx = %d, want %d", i, gi, i)
		}
	}
}

// --- accessor helpers ---------------------------------------------------

func mustGetNodeString(t *testing.T, g *lpg.Graph[string, int64], n, key, want string) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing node property %q on %q", key, n)
	}
	got, ok := v.String()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want String", n, key, v.Kind())
	}
	if got != want {
		t.Fatalf("%s.%s = %q, want %q", n, key, got, want)
	}
}

func mustGetNodeInt64(t *testing.T, g *lpg.Graph[string, int64], n, key string, want int64) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing %s.%s", n, key)
	}
	got, ok := v.Int64()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want Int64", n, key, v.Kind())
	}
	if got != want {
		t.Fatalf("%s.%s = %d, want %d", n, key, got, want)
	}
}

func mustGetNodeFloat64(t *testing.T, g *lpg.Graph[string, int64], n, key string, want float64) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing %s.%s", n, key)
	}
	got, ok := v.Float64()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want Float64", n, key, v.Kind())
	}
	if math.Float64bits(got) != math.Float64bits(want) {
		t.Fatalf("%s.%s = %g (bits=%#016x), want %g (bits=%#016x)",
			n, key, got, math.Float64bits(got), want, math.Float64bits(want))
	}
}

func mustGetNodeBool(t *testing.T, g *lpg.Graph[string, int64], n, key string, want bool) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing %s.%s", n, key)
	}
	got, ok := v.Bool()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want Bool", n, key, v.Kind())
	}
	if got != want {
		t.Fatalf("%s.%s = %v, want %v", n, key, got, want)
	}
}

func mustGetNodeTime(t *testing.T, g *lpg.Graph[string, int64], n, key string, want time.Time) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing %s.%s", n, key)
	}
	got, ok := v.Time()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want Time", n, key, v.Kind())
	}
	if !got.Equal(want) {
		t.Fatalf("%s.%s = %v, want %v", n, key, got, want)
	}
}

func mustGetNodeBytes(t *testing.T, g *lpg.Graph[string, int64], n, key string, want []byte) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, key)
	if !ok {
		t.Fatalf("missing %s.%s", n, key)
	}
	got, ok := v.Bytes()
	if !ok {
		t.Fatalf("%s.%s kind = %d, want Bytes", n, key, v.Kind())
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s.%s = %x, want %x", n, key, got, want)
	}
}

func mustGetEdgeString(t *testing.T, g *lpg.Graph[string, int64], src, dst, key, want string) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge property %s.%s->%s", src, key, dst)
	}
	got, ok := v.String()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want String", src, dst, key, v.Kind())
	}
	if got != want {
		t.Fatalf("edge(%s,%s).%s = %q, want %q", src, dst, key, got, want)
	}
}

func mustGetEdgeInt64(t *testing.T, g *lpg.Graph[string, int64], src, dst, key string, want int64) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge(%s,%s).%s", src, dst, key)
	}
	got, ok := v.Int64()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want Int64", src, dst, key, v.Kind())
	}
	if got != want {
		t.Fatalf("edge(%s,%s).%s = %d, want %d", src, dst, key, got, want)
	}
}

func mustGetEdgeFloat64(t *testing.T, g *lpg.Graph[string, int64], src, dst, key string, want float64) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge(%s,%s).%s", src, dst, key)
	}
	got, ok := v.Float64()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want Float64", src, dst, key, v.Kind())
	}
	if math.Float64bits(got) != math.Float64bits(want) {
		t.Fatalf("edge(%s,%s).%s = %g, want %g", src, dst, key, got, want)
	}
}

func mustGetEdgeBool(t *testing.T, g *lpg.Graph[string, int64], src, dst, key string, want bool) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge(%s,%s).%s", src, dst, key)
	}
	got, ok := v.Bool()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want Bool", src, dst, key, v.Kind())
	}
	if got != want {
		t.Fatalf("edge(%s,%s).%s = %v, want %v", src, dst, key, got, want)
	}
}

func mustGetEdgeTime(t *testing.T, g *lpg.Graph[string, int64], src, dst, key string, want time.Time) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge(%s,%s).%s", src, dst, key)
	}
	got, ok := v.Time()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want Time", src, dst, key, v.Kind())
	}
	if !got.Equal(want) {
		t.Fatalf("edge(%s,%s).%s = %v, want %v", src, dst, key, got, want)
	}
}

func mustGetEdgeBytes(t *testing.T, g *lpg.Graph[string, int64], src, dst, key string, want []byte) {
	t.Helper()
	v, ok := g.GetEdgeProperty(src, dst, key)
	if !ok {
		t.Fatalf("missing edge(%s,%s).%s", src, dst, key)
	}
	got, ok := v.Bytes()
	if !ok {
		t.Fatalf("edge(%s,%s).%s kind = %d, want Bytes", src, dst, key, v.Kind())
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("edge(%s,%s).%s = %x, want %x", src, dst, key, got, want)
	}
}
