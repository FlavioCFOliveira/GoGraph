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

// TestReadProperties_TruncatedHeader covers the early-return branches
// in ReadProperties: a stream shorter than the magic / version /
// count headers must surface as ErrPropertiesCorrupted rather than
// the underlying io.EOF.
func TestReadProperties_TruncatedHeader(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty":              {},
		"partial-magic":      {0x53, 0x50}, // 2 of 4 magic bytes
		"magic-only":         propsMagicBytes(),
		"magic+partial-ver":  append(propsMagicBytes(), 0x01),
		"magic+ver":          append(propsMagicBytes(), 1, 0, 0, 0),
		"magic+ver+badcount": append(propsMagicBytes(), 1, 0, 0, 0, 1), // partial keyCount
		"magic+badver":       append(propsMagicBytes(), 0xff, 0xff, 0xff, 0xff),
	}
	for name, payload := range cases {
		payload := payload
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadProperties(bytes.NewReader(payload))
			if !errors.Is(err, ErrPropertiesCorrupted) {
				t.Fatalf("ReadProperties(%s) = %v, want ErrPropertiesCorrupted", name, err)
			}
		})
	}
}

// TestReadProperties_ImplausibleCounts asserts that the uint64
// counters for the key table, node records, and edge records reject
// values past the documented sanity caps.
func TestReadProperties_ImplausibleCounts(t *testing.T) {
	t.Parallel()

	// Implausibly large key count.
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<31))
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("implausible key count = %v, want ErrPropertiesCorrupted", err)
	}

	// Implausibly large node count.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))     // no keys
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<41)) // > 1<<40
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("implausible node count = %v, want ErrPropertiesCorrupted", err)
	}

	// Implausibly large edge count.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no keys
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no node records
	_ = binary.Write(buf, binary.LittleEndian, uint64(1<<41))
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("implausible edge count = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_ImplausibleKeyLen asserts a single key whose
// length prefix is past 1<<20 is rejected rather than driving a huge
// allocation.
func TestReadProperties_ImplausibleKeyLen(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))     // one key
	_ = binary.Write(buf, binary.LittleEndian, uint32(1<<25)) // 32 MiB key — rejected at 1<<20 cap
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("implausible key length = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_KeyIdxOutOfRange asserts that a record pointing
// past the embedded key table is rejected rather than indexing
// out-of-bounds at apply time.
func TestReadProperties_KeyIdxOutOfRange(t *testing.T) {
	t.Parallel()

	// Node record with a bad keyIdx.
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // one key
	_ = binary.Write(buf, binary.LittleEndian, uint32(2)) // utf8Len
	_, _ = buf.WriteString("kx")
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // nodeCount
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // NodeID
	_ = binary.Write(buf, binary.LittleEndian, uint32(5)) // bad KeyIdx
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("node keyIdx out-of-range = %v, want ErrPropertiesCorrupted", err)
	}

	// Edge record with a bad keyIdx.
	buf = &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(2))
	_, _ = buf.WriteString("kx")
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no node records
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // 1 edge record
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(7)) // bad KeyIdx
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("edge keyIdx out-of-range = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_UnknownKind asserts an unknown kind tag in a
// record surfaces as corruption rather than a silent skip.
func TestReadProperties_UnknownKind(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1))
	_, _ = buf.WriteString("k")
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // one node record
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // NodeID
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // KeyIdx
	_ = buf.WriteByte(0xAA)                               // unknown kind
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // valueLen
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("unknown kind = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_WrongFixedLen asserts a fixed-width kind whose
// length prefix disagrees with the expected size is rejected. We
// stage a PropInt64 (kind=2, expected 8 bytes) with valueLen=4.
func TestReadProperties_WrongFixedLen(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1))
	_, _ = buf.WriteString("k")
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // node count
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // NodeID
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // KeyIdx
	_ = buf.WriteByte(byte(lpg.PropInt64))                // expects 8 bytes
	_ = binary.Write(buf, binary.LittleEndian, uint32(4)) // wrong length
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("wrong fixed length = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_ValueLenTooLarge enforces the 1 GiB cap on a
// single value's byte length.
func TestReadProperties_ValueLenTooLarge(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1))
	_, _ = buf.WriteString("k")
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
	_ = buf.WriteByte(byte(lpg.PropString))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1<<31)) // > 1 GiB
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("value len too large = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestApplyPropertiesToGraph_UnresolvedNodeID covers the
// metric-counted skip branch: a property record names a NodeID the
// live mapper has never interned. ApplyPropertiesToGraph must not
// error and must leave the graph unchanged.
func TestApplyPropertiesToGraph_UnresolvedNodeID(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	rb := PropertiesReadback{
		Keys: []string{"k"},
		NodeProperties: []NodePropertyEntry{
			{
				NodeID:     99999,
				KeyIdx:     0,
				Kind:       lpg.PropInt64,
				ValueBytes: int64Bytes(1),
			},
		},
		EdgeProperties: []EdgePropertyEntry{
			{
				Src:        99998,
				Dst:        99999,
				KeyIdx:     0,
				Kind:       lpg.PropInt64,
				ValueBytes: int64Bytes(1),
			},
		},
	}
	if err := ApplyPropertiesToGraph(g, rb); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}
	if props := g.NodeProperties("alice"); len(props) != 0 {
		t.Fatalf("alice should have no properties, got %v", props)
	}
}

// TestApplyPropertiesToGraph_MissingEdge covers the edge-missing
// skip branch: both endpoints resolve, but no adjacency entry
// connects them.
func TestApplyPropertiesToGraph_MissingEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("alice")
	dstID, _ := g.AdjList().Mapper().Lookup("bob")
	rb := PropertiesReadback{
		Keys: []string{"k"},
		EdgeProperties: []EdgePropertyEntry{
			{
				Src:        uint64(srcID),
				Dst:        uint64(dstID),
				KeyIdx:     0,
				Kind:       lpg.PropInt64,
				ValueBytes: int64Bytes(7),
			},
		},
	}
	if err := ApplyPropertiesToGraph(g, rb); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}
	if _, ok := g.GetEdgeProperty("alice", "bob", "k"); ok {
		t.Fatal("edge property must not be attached when the edge is absent")
	}
}

// TestApplyPropertiesToGraph_KeyIdxOutOfRange exercises the
// defensive bounds check inside ApplyPropertiesToGraph that runs
// even after ReadProperties has validated indexes.
func TestApplyPropertiesToGraph_KeyIdxOutOfRange(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("alice")
	rb := PropertiesReadback{
		Keys: []string{"k"},
		NodeProperties: []NodePropertyEntry{
			{NodeID: uint64(srcID), KeyIdx: 99, Kind: lpg.PropInt64, ValueBytes: int64Bytes(1)},
		},
		EdgeProperties: []EdgePropertyEntry{
			{Src: uint64(srcID), Dst: uint64(srcID), KeyIdx: 99, Kind: lpg.PropInt64, ValueBytes: int64Bytes(1)},
		},
	}
	if err := ApplyPropertiesToGraph(g, rb); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}
	if props := g.NodeProperties("alice"); len(props) != 0 {
		t.Fatalf("alice should have no properties, got %v", props)
	}
}

// TestApplyPropertiesToGraph_BadDecodeSkipped covers the path where
// a record carries a kind whose decoder fails (e.g., wrong length on
// a fixed-width kind that slipped past ReadProperties). The apply
// function must skip the record without erroring.
func TestApplyPropertiesToGraph_BadDecodeSkipped(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("alice")
	rb := PropertiesReadback{
		Keys: []string{"k"},
		NodeProperties: []NodePropertyEntry{
			{
				NodeID:     uint64(srcID),
				KeyIdx:     0,
				Kind:       lpg.PropInt64,
				ValueBytes: []byte{0x01}, // wrong size — decoder rejects
			},
		},
	}
	if err := ApplyPropertiesToGraph(g, rb); err != nil {
		t.Fatalf("ApplyPropertiesToGraph: %v", err)
	}
	if _, ok := g.GetNodeProperty("alice", "k"); ok {
		t.Fatal("alice.k must not have been applied with a malformed value")
	}
}

// TestWriteProperties_WriterFailure feeds a writer that rejects
// every byte and verifies WriteProperties surfaces the error.
func TestWriteProperties_WriterFailure(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeProperty("a", "k", lpg.Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	w := errWriter{err: errors.New("write boom")}
	_, _, err := WriteProperties(w, g)
	if err == nil {
		t.Fatal("WriteProperties on a failing writer must return an error")
	}
}

// TestWriteProperties_FlushFailure exercises the bufio.Flush error
// path. A partialWriter that accepts 0 bytes returns an error on
// the very first underlying write; WriteProperties stages everything
// in the in-memory bufio.Writer until the final Flush, which is
// where the failure surfaces.
func TestWriteProperties_FlushFailure(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty("n", "k", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	w := &partialWriter{n: 0, err: errors.New("flush boom")}
	_, _, err := WriteProperties(w, g)
	if err == nil {
		t.Fatal("WriteProperties must surface the underlying writer's failure")
	}
}

// TestWriteProperties_RoundtripBytesAndStrings covers the edge case
// where node and edge properties carry variable-width payloads of
// non-trivial length, exercising the writer's record-size accounting.
func TestWriteProperties_RoundtripBytesAndStrings(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	long := bytes.Repeat([]byte{0xAB}, 4096)
	longStr := string(bytes.Repeat([]byte{'x'}, 4096))
	if err := g.SetNodeProperty("a", "blob", lpg.BytesValue(long)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("a", "tag", lpg.StringValue(longStr)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	g.SetEdgeProperty("a", "b", "raw", lpg.BytesValue(long))

	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := restored.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := ApplyPropertiesToGraph(restored, loaded.Properties); err != nil {
		t.Fatal(err)
	}
	v, ok := restored.GetNodeProperty("a", "blob")
	if !ok {
		t.Fatal("missing a.blob")
	}
	if b, _ := v.Bytes(); !bytes.Equal(b, long) {
		t.Fatalf("a.blob mismatch (len=%d)", len(b))
	}
	v, ok = restored.GetNodeProperty("a", "tag")
	if !ok {
		t.Fatal("missing a.tag")
	}
	if s, _ := v.String(); s != longStr {
		t.Fatalf("a.tag mismatch (len=%d)", len(s))
	}
	v, ok = restored.GetEdgeProperty("a", "b", "raw")
	if !ok {
		t.Fatal("missing edge(a,b).raw")
	}
	if b, _ := v.Bytes(); !bytes.Equal(b, long) {
		t.Fatalf("edge raw mismatch (len=%d)", len(b))
	}
}

// TestReadProperties_TruncatedNodeRecord asserts a node record cut
// off mid-value-bytes surfaces as ErrPropertiesCorrupted (covers the
// io.ReadFull tail in readNodePropRecord).
func TestReadProperties_TruncatedNodeRecord(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1))
	_, _ = buf.WriteString("k")
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // 1 node record
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // NodeID
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // KeyIdx
	_ = buf.WriteByte(byte(lpg.PropString))
	_ = binary.Write(buf, binary.LittleEndian, uint32(10))
	_, _ = buf.WriteString("xy") // only 2 bytes of the promised 10
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("truncated node record = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_TruncatedEdgeRecord mirrors the node test for the
// edge record reader path.
func TestReadProperties_TruncatedEdgeRecord(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(1))
	_, _ = buf.WriteString("k")
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no node records
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // 1 edge record
	_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // Src
	_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // Dst
	_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // KeyIdx
	_ = buf.WriteByte(byte(lpg.PropBytes))
	_ = binary.Write(buf, binary.LittleEndian, uint32(20))
	_, _ = buf.WriteString("xy")
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("truncated edge record = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestReadProperties_TruncatedKey asserts a key whose utf-8 byte run
// is cut short surfaces as corruption.
func TestReadProperties_TruncatedKey(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.LittleEndian, propertiesMagic)
	_ = binary.Write(buf, binary.LittleEndian, propertiesFormatVersion)
	_ = binary.Write(buf, binary.LittleEndian, uint64(1))  // one key
	_ = binary.Write(buf, binary.LittleEndian, uint32(20)) // utf8Len = 20
	_, _ = buf.WriteString("xy")                           // but only 2 bytes follow
	if _, err := ReadProperties(buf); !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("truncated key = %v, want ErrPropertiesCorrupted", err)
	}
}

// TestWriteProperties_RoundtripEmptyKeys covers the writer / reader
// degenerate case where a key has zero bytes. The reader must
// accept an empty utf-8 string.
func TestWriteProperties_RoundtripEmptyKeys(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeProperty("a", "", lpg.Int64Value(7)); err != nil { // empty key name
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
	if len(loaded.Properties.Keys) != 1 || loaded.Properties.Keys[0] != "" {
		t.Fatalf("Keys = %v, want [\"\"]", loaded.Properties.Keys)
	}
}

// TestLoadSnapshotFull_MissingPropertiesBin exercises the path where
// the manifest claims a properties.bin entry but the file is
// missing on disk. LoadSnapshotFull must surface an os.Open error.
func TestLoadSnapshotFull_MissingPropertiesBin(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeProperty("a", "k", lpg.Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, PropertiesFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshotFull(dir); err == nil {
		t.Fatal("LoadSnapshotFull with missing properties.bin should error")
	}
}

// TestWriteSnapshotFullCtx_FlakyCtxAfterProperties exercises the
// post-properties-write context check.
func TestWriteSnapshotFullCtx_FlakyCtxAfterProperties(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeProperty("a", "k", lpg.Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	sentinel := errors.New("ctx flakes after properties")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		// 4 nil-Err()s clear (pre-CSR, post-CSR, post-labels,
		// post-properties); the 5th call (pre-manifest checkpoint)
		// trips. The pre-manifest ctx.Err() check happens implicitly
		// inside the manifest write block — the next Err() call is
		// the pre-rename one, which is where the sentinel will fire.
		nilCalls: 4,
		err:      sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullCtx(ctx, dir, c, g); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotFullCtx post-properties flake = %v, want %v", err, sentinel)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should not exist, stat=%v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp should be cleaned, stat=%v", err)
	}
}

// propsMagicBytes returns the 4-byte little-endian magic header used
// by properties.bin. Convenient when building synthetic streams for
// the truncated-header tests.
func propsMagicBytes() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, propertiesMagic)
	return b
}

// int64Bytes returns the LE 8-byte encoding of i.
func int64Bytes(i int64) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(i))
	return out
}
