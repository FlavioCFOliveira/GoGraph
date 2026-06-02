package snapshot

// tombstones_test.go — durability coverage for the tombstones.bin component.
// The pre-existing snapshot suite never round-tripped the node-removal set,
// so a deleted node silently resurrected on reopen. Every test here writes
// the set to disk (or to the wire), DISCARDS the source graph, reads it
// back into a FRESH graph, and asserts the tombstone set survived.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildTombstoneGraph returns a string/int64 graph with three labelled,
// propertied nodes (a, b, c) where a and c have been removed, so the live
// set is {b} and the tombstone set is {id(a), id(c)}.
func buildTombstoneGraph(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, k := range []string{"a", "b", "c"} {
		if err := g.SetNodeLabel(k, "Spec"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "key", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", k, err)
		}
	}
	// Delete a and c the way the engine does: strip then tombstone.
	for _, k := range []string{"a", "c"} {
		for _, lbl := range g.NodeLabels(k) {
			g.RemoveNodeLabel(k, lbl)
		}
		for pk := range g.NodeProperties(k) {
			g.DelNodeProperty(k, pk)
		}
		g.RemoveNode(k)
	}
	return g
}

// TestWriteReadTombstones_RoundTrip locks the codec: the sorted id set
// written by WriteTombstones is parsed back identically by ReadTombstones.
func TestWriteReadTombstones_RoundTrip(t *testing.T) {
	t.Parallel()
	g := buildTombstoneGraph(t)
	want := g.TombstonedIDs()
	if len(want) != 2 {
		t.Fatalf("precondition: tombstone count = %d, want 2", len(want))
	}

	var buf bytes.Buffer
	size, _, err := WriteTombstones(&buf, g)
	if err != nil {
		t.Fatalf("WriteTombstones: %v", err)
	}
	if int(size) != buf.Len() {
		t.Fatalf("declared size %d != bytes written %d", size, buf.Len())
	}

	rb, err := ReadTombstones(&buf)
	if err != nil {
		t.Fatalf("ReadTombstones: %v", err)
	}
	if len(rb.IDs) != len(want) {
		t.Fatalf("read %d ids, want %d", len(rb.IDs), len(want))
	}
	for i := range want {
		if rb.IDs[i] != want[i] {
			t.Fatalf("id[%d] = %d, want %d (set %v)", i, rb.IDs[i], want[i], rb.IDs)
		}
	}
}

// TestReadTombstones_Corruption rejects structurally malformed payloads.
func TestReadTombstones_Corruption(t *testing.T) {
	t.Parallel()
	// A valid 1-id payload to derive corruptions from.
	good := func() []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.LittleEndian, tombstonesMagic)
		_ = binary.Write(&b, binary.LittleEndian, tombstonesFormatVersion)
		_ = binary.Write(&b, binary.LittleEndian, uint64(1))
		_ = binary.Write(&b, binary.LittleEndian, uint64(42))
		return b.Bytes()
	}

	cases := map[string][]byte{
		"bad magic": func() []byte {
			b := good()
			b[0] ^= 0xFF
			return b
		}(),
		"unsupported version": func() []byte {
			var b bytes.Buffer
			_ = binary.Write(&b, binary.LittleEndian, tombstonesMagic)
			_ = binary.Write(&b, binary.LittleEndian, uint32(999))
			_ = binary.Write(&b, binary.LittleEndian, uint64(0))
			return b.Bytes()
		}(),
		"short header":      good()[:6],
		"truncated id body": good()[:len(good())-3],
		"implausible count": func() []byte {
			var b bytes.Buffer
			_ = binary.Write(&b, binary.LittleEndian, tombstonesMagic)
			_ = binary.Write(&b, binary.LittleEndian, tombstonesFormatVersion)
			_ = binary.Write(&b, binary.LittleEndian, tombstonesMaxCount+1)
			return b.Bytes()
		}(),
	}
	for name, payload := range cases {
		payload := payload
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadTombstones(bytes.NewReader(payload)); !errors.Is(err, ErrTombstonesCorrupted) {
				t.Fatalf("ReadTombstones(%s) error = %v, want ErrTombstonesCorrupted", name, err)
			}
		})
	}
}

// TestSnapshot_TombstonesRoundTrip is the full-component durability test:
// write a snapshot of a graph with deletions, load it, apply it into a
// FRESH graph, and assert the tombstone set, live count, live node state,
// and scan exclusion all survived.
func TestSnapshot_TombstonesRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "snapshot")
	g := buildTombstoneGraph(t)
	idA, _ := g.AdjList().Mapper().Lookup("a")
	idB, _ := g.AdjList().Mapper().Lookup("b")

	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Tombstones.IDs) != 2 {
		t.Fatalf("loaded tombstones = %d, want 2", len(loaded.Tombstones.IDs))
	}

	// Apply into a fresh graph the way recovery does (no WAL).
	fresh := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(fresh, loaded.Mapper); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}
	if err := ApplyCSRToGraph(fresh, &loaded.CSR); err != nil {
		t.Fatalf("ApplyCSRToGraph: %v", err)
	}
	if err := ApplyLabelsToGraph(fresh, loaded.Labels); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	if err := ApplyPropertiesToGraph(fresh, loaded.Properties); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}
	ApplyTombstonesToGraph(fresh, loaded.Tombstones)

	if !fresh.IsTombstoned(idA) {
		t.Error("node a must be tombstoned after snapshot round-trip")
	}
	if fresh.IsTombstoned(idB) {
		t.Error("node b must be live after snapshot round-trip")
	}
	if got := fresh.LiveOrder(); got != 1 {
		t.Errorf("LiveOrder = %d, want 1 (only b is live)", got)
	}
	// The surviving live node keeps its label and property.
	if !fresh.HasNodeLabel("b", "Spec") {
		t.Error("live node b lost its label across the round-trip")
	}
	if v, ok := fresh.GetNodeProperty("b", "key"); !ok {
		t.Error("live node b lost its property across the round-trip")
	} else if s, _ := v.String(); s != "b" {
		t.Errorf("b.key = %q, want \"b\"", s)
	}
}

// TestSnapshot_BackCompat_NoTombstonesComponent asserts that a graph that
// never deleted a node produces NO tombstones.bin (byte-identical to the
// pre-feature layout) and that the loader yields an empty set — the
// backward-compatibility contract for old snapshots.
func TestSnapshot_BackCompat_NoTombstonesComponent(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "snapshot")
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("x", "Spec"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, TombstonesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstones.bin must be absent for a never-deleting graph (stat err=%v)", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	for _, f := range loaded.Manifest.Files {
		if f.Name == TombstonesFile {
			t.Fatal("manifest must not list tombstones.bin for a never-deleting graph")
		}
	}
	if len(loaded.Tombstones.IDs) != 0 {
		t.Fatalf("loaded tombstones = %d, want 0 (back-compat empty set)", len(loaded.Tombstones.IDs))
	}
}

// TestSnapshot_TombstonesCRCRejected confirms a corrupted tombstones.bin is
// rejected at load via the manifest CRC, like every other component.
func TestSnapshot_TombstonesCRCRejected(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "snapshot")
	g := buildTombstoneGraph(t)
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	path := filepath.Join(dir, TombstonesFile)
	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("tombstones.bin unexpectedly empty")
	}
	raw[len(raw)-1] ^= 0xFF // flip a byte in the last id
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := LoadSnapshotFull(dir); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("LoadSnapshotFull on corrupted tombstones.bin error = %v, want ErrCorrupted", err)
	}
}
