package btree

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// wire_identity_test.go — task #1514 storage-engine-auditor condition C2:
// the B+ tree must serialise to BYTE-IDENTICAL output to the v1 sorted-array
// writer. The wire format encodes a logical sorted key→ascending-NodeID-set
// mapping, not the physical tree shape, so the bytes are fully determined by
// the logical content. These tests pin that determinism so a future structural
// change cannot silently alter the on-disk format (which old snapshots and any
// v1 reader depend on).

// craftGoldenString builds the EXACT v1 wire image for the given (key, ids)
// entries by hand — independent of any Index code — so it is an external oracle
// for byte-identity. Entries must be supplied in ascending key order with
// ascending ids.
func craftGoldenString(t *testing.T, entries []struct {
	key string
	ids []uint64
}) []byte {
	t.Helper()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, btreeMagic)
	_ = binary.Write(&body, binary.LittleEndian, btreeFormatVersion)
	_ = binary.Write(&body, binary.LittleEndian, uint64(len(entries)))
	for _, e := range entries {
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(e.key)))
		body.WriteString(e.key)
		_ = binary.Write(&body, binary.LittleEndian, uint64(len(e.ids)))
		_ = binary.Write(&body, binary.LittleEndian, e.ids)
	}
	sum := crc32.Checksum(body.Bytes(), castagnoli)
	var out bytes.Buffer
	out.Write(body.Bytes())
	_ = binary.Write(&out, binary.LittleEndian, sum)
	return out.Bytes()
}

// TestSerialize_GoldenBytes_String asserts the B+ tree writer emits the exact
// hand-crafted v1 image. This is the regression gate that the format version
// stays 1 and the byte layout is unchanged (auditor C1/C2).
func TestSerialize_GoldenBytes_String(t *testing.T) {
	t.Parallel()
	// Use enough distinct keys to force several leaf splits at fanout 128, so
	// the multi-leaf chain is exercised by the in-order walk.
	const n = 200
	idx := New[string]()
	want := make([]struct {
		key string
		ids []uint64
	}, 0, n)
	for i := 0; i < n; i++ {
		k := "key-" + itoaPad(i)
		// Insert in shuffled-ish order to exercise tree structure, two ids
		// per key.
		idx.Insert(k, graph.NodeID(uint64(i)))
		idx.Insert(k, graph.NodeID(uint64(i+10_000)))
		want = append(want, struct {
			key string
			ids []uint64
		}{key: k, ids: []uint64{uint64(i), uint64(i + 10_000)}})
	}

	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	golden := craftGoldenString(t, want)
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Fatalf("serialised bytes differ from the hand-crafted v1 image\n got %d bytes\nwant %d bytes", buf.Len(), len(golden))
	}
}

// TestSerialize_Deterministic_AcrossBuildOrder asserts that two indexes with
// identical logical content serialise to identical bytes regardless of how they
// were built (random Insert order vs BulkLoad). The tree shape may differ; the
// wire image must not.
func TestSerialize_Deterministic_AcrossBuildOrder(t *testing.T) {
	t.Parallel()
	const n = 500

	// Build A via random-order Insert.
	a := New[int64]()
	for _, k := range benchKeys(n) {
		a.Insert(k, graph.NodeID(uint64(k)))
	}
	// Build B via BulkLoad.
	vals := make([]int64, n)
	nodes := make([]graph.NodeID, n)
	for i := 0; i < n; i++ {
		vals[i] = int64(i)
		nodes[i] = graph.NodeID(uint64(i))
	}
	b := New[int64]()
	if err := b.BulkLoad(vals, nodes); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	var ba, bb bytes.Buffer
	if err := a.Serialize(&ba); err != nil {
		t.Fatalf("Serialize A: %v", err)
	}
	if err := b.Serialize(&bb); err != nil {
		t.Fatalf("Serialize B: %v", err)
	}
	if !bytes.Equal(ba.Bytes(), bb.Bytes()) {
		t.Fatalf("Insert-built and BulkLoad-built indexes with identical content serialised differently (%d vs %d bytes)", ba.Len(), bb.Len())
	}
}

// TestLookup_CloneIsIndependent pins the graph-theory-expert caveat (b): the
// bitmap returned by Lookup is a deep clone, so a later writer mutating the
// index's stored bitmap does not affect the caller's copy.
func TestLookup_CloneIsIndependent(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(5, graph.NodeID(50))
	idx.Insert(5, graph.NodeID(55))

	got := idx.Lookup(5)
	if got.GetCardinality() != 2 {
		t.Fatalf("Lookup(5) cardinality = %d, want 2", got.GetCardinality())
	}
	// Mutate the index after the clone was taken.
	idx.Insert(5, graph.NodeID(99))
	idx.Delete(5, graph.NodeID(50))
	// The clone must be unchanged.
	if got.GetCardinality() != 2 || !got.Contains(50) || !got.Contains(55) || got.Contains(99) {
		t.Fatalf("Lookup clone aliased the index's bitmap: cardinality=%d", got.GetCardinality())
	}
}
