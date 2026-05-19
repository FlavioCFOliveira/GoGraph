package snapshot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
)

// TestReadLabels_TruncatedHeader covers the early-return branches in
// ReadLabels: a stream shorter than the magic / version / count
// headers must surface as ErrLabelsCorrupted, not as a parse-error
// passthrough.
func TestReadLabels_TruncatedHeader(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty":               {},
		"partial-magic":       {0x53, 0x4C}, // only 2 of 4 magic bytes
		"magic-only":          magicBytes(),
		"magic+partial-ver":   append(magicBytes(), 0x01),
		"magic+ver":           append(magicBytes(), 1, 0, 0, 0),
		"magic+ver+badcount":  append(magicBytes(), 1, 0, 0, 0, 1), // truncated stringTableLen
		"magic+badver":        append(magicBytes(), 0xff, 0xff, 0xff, 0xff),
		"magic+ver+strcount0": appendUint64(append(magicBytes(), 1, 0, 0, 0), 0),
	}
	for name, payload := range cases {
		payload := payload
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadLabels(bytes.NewReader(payload))
			if name == "magic+ver+strcount0" {
				// This one is structurally complete only through the
				// stringTableLen=0 marker; it then trips on the
				// missing nodeCount header. Still must be
				// ErrLabelsCorrupted.
				if !errors.Is(err, ErrLabelsCorrupted) {
					t.Fatalf("ReadLabels(%s) = %v, want ErrLabelsCorrupted", name, err)
				}
				return
			}
			if !errors.Is(err, ErrLabelsCorrupted) {
				t.Fatalf("ReadLabels(%s) = %v, want ErrLabelsCorrupted", name, err)
			}
		})
	}
}

// TestReadLabels_ImplausibleCounts asserts that the uint64 counters
// for the string table, node records, and edge records reject
// values past the documented sanity caps. This protects readers
// from blowing up memory on a corrupted file that claims it
// contains 1<<63 records.
func TestReadLabels_ImplausibleCounts(t *testing.T) {
	t.Parallel()
	// Build a synthetic stream: magic + version + stringTableLen=1<<31
	// (the writer's 1<<30 cap is exclusive, so 1<<31 trips it).
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
	_ = binary.Write(buf, binary.LittleEndian, labelsFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<31))
	if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("implausible string count = %v, want ErrLabelsCorrupted", err)
	}

	// Same for node count: write a valid string table prefix, then a
	// huge nodeCount.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
	_ = binary.Write(buf, binary.LittleEndian, labelsFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))     // no strings
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<41)) // > 1<<40
	if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("implausible node count = %v, want ErrLabelsCorrupted", err)
	}

	// Same for edge count.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
	_ = binary.Write(buf, binary.LittleEndian, labelsFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // 0 node records
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<41))
	if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("implausible edge count = %v, want ErrLabelsCorrupted", err)
	}
}

// TestReadLabels_StringIdxOutOfRange asserts that a record pointing
// past the embedded string table is rejected rather than indexing
// out-of-bounds at apply time.
func TestReadLabels_StringIdxOutOfRange(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
	_ = binary.Write(buf, binary.LittleEndian, labelsFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // one string
	_ = binary.Write(buf, binary.LittleEndian, uint32(2)) // utf8Len
	_, _ = buf.WriteString("Px")
	// One node record whose stringIdx (5) is out of range (only 0
	// is valid for a 1-entry table).
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // nodeCount
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // NodeID
	_ = binary.Write(buf, binary.LittleEndian, uint32(5)) // bad StringIdx
	if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("node string idx out-of-range = %v, want ErrLabelsCorrupted", err)
	}

	// Same for an edge record.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
	_ = binary.Write(buf, binary.LittleEndian, labelsFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(2))
	_, _ = buf.WriteString("Px")
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no node records
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // 1 edge record
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(7))
	if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
		t.Fatalf("edge string idx out-of-range = %v, want ErrLabelsCorrupted", err)
	}
}

// TestApplyLabelsToGraph_UnresolvedNodeID covers the metric-counted
// skip branch: a label record names a NodeID the live mapper has
// never interned. ApplyLabelsToGraph must not error and must leave
// the graph unchanged.
func TestApplyLabelsToGraph_UnresolvedNodeID(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddNode("alice") // mapper has exactly one node
	rb := LabelsReadback{
		Strings: []string{"Ghost"},
		NodeLabels: []NodeLabelEntry{
			{NodeID: 99999, StringIdx: 0}, // unresolvable
		},
		EdgeLabels: []EdgeLabelEntry{
			{Src: 99998, Dst: 99999, StringIdx: 0}, // both unresolvable
		},
	}
	if err := ApplyLabelsToGraph(g, rb); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	if labs := g.NodeLabels("alice"); len(labs) != 0 {
		t.Fatalf("alice should have no labels, got %v", labs)
	}
}

// TestApplyLabelsToGraph_MissingEdge covers the edge-missing skip
// branch: both endpoints resolve, but no adjacency entry connects
// them. ApplyLabelsToGraph must skip the record (no panic, no
// error).
func TestApplyLabelsToGraph_MissingEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddNode("alice")
	g.AddNode("bob")
	srcID, _ := g.AdjList().Mapper().Lookup("alice")
	dstID, _ := g.AdjList().Mapper().Lookup("bob")
	rb := LabelsReadback{
		Strings: []string{"KNOWS"},
		EdgeLabels: []EdgeLabelEntry{
			{Src: uint64(srcID), Dst: uint64(dstID), StringIdx: 0},
		},
	}
	if err := ApplyLabelsToGraph(g, rb); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	if g.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("edge label must not be attached when the edge is absent")
	}
}

// TestApplyLabelsToGraph_StringIdxOutOfRange exercises the
// defensive bounds check inside ApplyLabelsToGraph that runs even
// after ReadLabels has already validated indexes — the function is
// callable with a manually constructed readback, so it must not
// trust the input blindly.
func TestApplyLabelsToGraph_StringIdxOutOfRange(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddNode("alice")
	srcID, _ := g.AdjList().Mapper().Lookup("alice")
	rb := LabelsReadback{
		Strings: []string{"A"},
		NodeLabels: []NodeLabelEntry{
			{NodeID: uint64(srcID), StringIdx: 99},
		},
		EdgeLabels: []EdgeLabelEntry{
			{Src: uint64(srcID), Dst: uint64(srcID), StringIdx: 99},
		},
	}
	if err := ApplyLabelsToGraph(g, rb); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	if labs := g.NodeLabels("alice"); len(labs) != 0 {
		t.Fatalf("alice should have no labels, got %v", labs)
	}
}

// TestWriteSnapshotFullCtx_CancelledContext covers the
// pre-write context-error path. Any non-nil err returned must
// match ctx.Err and the staging directory must be cleaned up.
func TestWriteSnapshotFullCtx_CancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	err := WriteSnapshotFullCtx(ctx, dir, c, g)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteSnapshotFullCtx(cancelled) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(dir + ".tmp"); !os.IsNotExist(statErr) && statErr == nil {
		// Pre-MkdirAll cancellation leaves no tmp; post-MkdirAll
		// cancellation cleans it up. Either way the .tmp must not
		// linger.
		t.Fatalf(".tmp directory left behind after cancellation: %v", statErr)
	}
}

// TestWriteSnapshotFullCtx_FlakyCtxAfterCSR exercises the
// post-CSR-write context check: the writer must clean up the
// staging directory and surface the sentinel error rather than
// publishing a half-built snapshot.
func TestWriteSnapshotFullCtx_FlakyCtxAfterCSR(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())

	sentinel := errors.New("ctx flakes after csr")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  1, // first Err() returns nil; subsequent calls trip
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(ctx, dir, c, g); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotFullCtx flaky-after-csr = %v, want %v", err, sentinel)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should not exist, stat=%v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp should be cleaned, stat=%v", err)
	}
}

// TestWriteSnapshotFullCtx_FlakyCtxAfterLabels exercises the
// post-labels-write context check.
func TestWriteSnapshotFullCtx_FlakyCtxAfterLabels(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())

	sentinel := errors.New("ctx flakes after labels")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  2, // first two Err()s clear; third trips
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(ctx, dir, c, g); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotFullCtx flaky-after-labels = %v, want %v", err, sentinel)
	}
}

// TestWriteSnapshotFullCtx_FlakyCtxBeforeRename exercises the final
// context check, just before the os.Rename publish step.
func TestWriteSnapshotFullCtx_FlakyCtxBeforeRename(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())

	sentinel := errors.New("ctx flakes pre-rename")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  3, // first three Err()s clear; fourth trips
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(ctx, dir, c, g); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotFullCtx pre-rename cancel = %v, want %v", err, sentinel)
	}
}

// TestWriteSnapshotFullCtx_OverwritesExisting verifies that a v2
// write into a directory that already exists (e.g., from a prior
// snapshot) replaces the previous content atomically: the
// publish-via-rename protocol must remove the old dir before
// renaming the .tmp into place.
func TestWriteSnapshotFullCtx_OverwritesExisting(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(context.Background(), dir, c, g); err != nil {
		t.Fatal(err)
	}
	// Re-write with an additional label.
	g.SetNodeLabel("b", "Y")
	c2 := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFullCtx(context.Background(), dir, c2, g); err != nil {
		t.Fatalf("second write: %v", err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Labels.NodeLabels) != 2 {
		t.Fatalf("NodeLabels = %d, want 2 after overwrite", len(loaded.Labels.NodeLabels))
	}
}

// TestWriteSnapshotFullCtx_CSRPath_AtomicPublish smoke-tests the
// directory hygiene of the v2 writer: after success the dir exists,
// the .tmp sibling is gone, and the manifest references both
// component files.
func TestWriteSnapshotFullCtx_AtomicPublish(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(context.Background(), dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFullCtx: %v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp dir should be removed, stat=%v", err)
	}
	m, err := ReadManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != ManifestVersion {
		t.Fatalf("manifest version %d, want %d", m.Version, ManifestVersion)
	}
	if len(m.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(m.Files))
	}
}

// TestLoadSnapshotFull_MissingCSR pins the contract that a manifest
// without a csr.bin entry surfaces as ErrCorrupted, even if a
// labels.bin entry is present (which is impossible in a valid
// directory but should still fail loudly).
func TestLoadSnapshotFull_MissingCSR(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := Manifest{
		Version: ManifestVersion,
		Files:   []FileEntry{}, // empty
	}
	mf, _ := os.Create(filepath.Join(dir, "manifest.json")) //nolint:gosec,errcheck // t.TempDir
	_ = WriteManifest(mf, m)
	_ = mf.Close()
	_, err := LoadSnapshotFull(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("LoadSnapshotFull(missing csr) = %v, want ErrCorrupted", err)
	}
}

// magicBytes returns the 4-byte little-endian magic header used by
// labels.bin. It is convenient when building synthetic streams for
// the truncated-header tests.
func magicBytes() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, labelsMagic)
	return b
}

// appendUint64 returns a fresh slice with v appended in LE.
func appendUint64(b []byte, v uint64) []byte {
	out := make([]byte, len(b)+8)
	copy(out, b)
	binary.LittleEndian.PutUint64(out[len(b):], v)
	return out
}

// TestWriteLabels_WriterFailureAtMagic exercises the very first
// error branch: a writer that refuses every byte must surface the
// failure from WriteLabels rather than panicking or silently
// truncating.
func TestWriteLabels_WriterFailureAtMagic(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "L")
	w := errWriter{err: errors.New("write boom")}
	_, _, err := WriteLabels(w, g)
	if err == nil {
		t.Fatal("WriteLabels on a failing writer must return an error")
	}
}

// TestWriteSnapshotFull_ParentIsAFile pins the failure mode where
// the parent of dir is occupied by a regular file: os.MkdirAll
// returns ENOTDIR (or equivalent), and WriteSnapshotFull surfaces
// that error rather than silently misplacing the snapshot.
func TestWriteSnapshotFull_ParentIsAFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(blocker, "snap")
	if err := WriteSnapshotFull(dir, c, g); err == nil {
		t.Fatal("WriteSnapshotFull(parent-is-file) should error")
	}
}

// TestLoadSnapshotFull_MissingLabelsBin exercises the path where the
// manifest claims a labels.bin entry but the file is absent on disk.
// LoadSnapshotFull must surface a filesystem open error rather than
// silently returning an empty readback.
func TestLoadSnapshotFull_MissingLabelsBin(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	// Remove labels.bin while leaving the manifest entry referring
	// to it. LoadSnapshotFull must surface an os.Open failure.
	if err := os.Remove(filepath.Join(dir, LabelsFile)); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSnapshotFull(dir)
	if err == nil {
		t.Fatal("LoadSnapshotFull with missing labels.bin should error")
	}
}

// TestLoadSnapshotFull_CorruptedCSR mirrors the labels-corruption
// test on the CSR side: a bit-flip in csr.bin must surface as
// ErrCorrupted through the v2 helper.
func TestLoadSnapshotFull_CorruptedCSR(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetNodeLabel("a", "X")
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(dir, CSRFile)
	data, err := os.ReadFile(csrPath) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(csrPath, data, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	_, err = LoadSnapshotFull(dir)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("LoadSnapshotFull(corrupted csr) = %v, want ErrCorrupted", err)
	}
}

// TestWriteLabels_FlushFailure exercises the bufio.Flush error path.
// A partialWriter that accepts 0 bytes returns an error on the very
// first underlying write; WriteLabels stages everything in the
// in-memory bufio.Writer until the final Flush, which is where the
// failure surfaces.
func TestWriteLabels_FlushFailure(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		// Build a graph fat enough that the bufio.Writer eventually
		// flushes mid-emit (the buffer is 1MiB so we cannot reach
		// it; instead the failure surfaces at the final Flush — that
		// is the canonical bufio behaviour and exactly what the test
		// asserts).
		g.AddNode("n")
		g.SetNodeLabel("n", "L")
	}
	w := &partialWriter{n: 0, err: errors.New("flush boom")}
	_, _, err := WriteLabels(w, g)
	if err == nil {
		t.Fatal("WriteLabels must surface the underlying writer's failure")
	}
}
