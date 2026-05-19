package recovery

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// encodeLegacyV1 builds a v1 (untagged) op payload by hand so the test
// does not rely on the txn package's internal encoder. The layout
// mirrors the format documented on [txn.encodeOpLegacy].
func encodeLegacyV1(kind txn.OpKind, src, dst, label string) []byte {
	buf := make([]byte, 0, 1+2+len(src)+2+len(dst)+2+len(label))
	buf = append(buf, byte(kind))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(src)))
	buf = append(buf, src...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(dst)))
	buf = append(buf, dst...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(label)))
	buf = append(buf, label...)
	return buf
}

// TestTxn_LegacyV1Replay verifies that a WAL composed entirely of
// hand-crafted v1 frames is replayed cleanly by the new recovery
// path. The fixture is constructed without going through
// [txn.NewStore] so any regression in the v1 reader is caught even if
// the writer changes.
func TestTxn_LegacyV1Replay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	frames := [][]byte{
		encodeLegacyV1(txn.OpSetNodeLabel, "alice", "", "Person"),
		encodeLegacyV1(txn.OpSetNodeLabel, "bob", "", "Person"),
		encodeLegacyV1(txn.OpAddEdge, "alice", "bob", ""),
		encodeLegacyV1(txn.OpSetEdgeLabel, "alice", "bob", "KNOWS"),
	}
	for _, p := range frames {
		if err := w.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.WALOps != len(frames) {
		t.Fatalf("WALOps = %d, want %d", res.WALOps, len(frames))
	}
	if !res.Graph.HasNodeLabel("alice", "Person") {
		t.Fatal("alice Person not applied")
	}
	if !res.Graph.HasNodeLabel("bob", "Person") {
		t.Fatal("bob Person not applied")
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("alice->bob edge not applied")
	}
	if !res.Graph.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("alice->bob KNOWS label not applied")
	}
}

// writeV2Workload commits a small mixed workload via a typed-codec
// store and closes the WAL. It is shared by the v2 replay tests.
func writeV2Workload(t *testing.T, dir string, codec txn.Codec[string]) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec[string, int64](g, w, codec)
	for _, name := range []string{"alice", "bob", "carol"} {
		tx := store.Begin()
		if err := tx.SetNodeLabel(name, "Person"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	tx := store.Begin()
	if err := tx.AddEdge("alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// assertV2GraphState fails the test if the recovered graph is missing
// any of the mutations produced by [writeV2Workload]. The expected
// WALOps count is the number of committed ops (5).
func assertV2GraphState(t *testing.T, g *lpg.Graph[string, int64], ops int) {
	t.Helper()
	if ops != 5 {
		t.Fatalf("WALOps = %d, want 5", ops)
	}
	if !g.HasNodeLabel("alice", "Person") {
		t.Fatal("alice Person missing")
	}
	if !g.AdjList().HasEdge("alice", "bob") {
		t.Fatal("alice->bob missing")
	}
	if !g.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("alice->bob KNOWS missing")
	}
}

// TestTxn_V2Replay round-trips a typed-codec store through commit and
// recovery using the new tagged frame path. The fixture instantiates
// the canonical StringCodec so the v2 layout is exercised end-to-end.
func TestTxn_V2Replay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	codec := txn.NewStringCodec()
	writeV2Workload(t, dir, codec)

	// OpenString recognises v2-StringCodec frames; verify state.
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	assertV2GraphState(t, res.Graph, res.WALOps)

	// OpenWithCodec walks the same WAL through the typed codec.
	res2, err := OpenWithCodec[string, int64](dir, codec)
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	assertV2GraphState(t, res2.Graph, res2.WALOps)
}

// TestTxn_V2Replay_BinaryMarshaler exercises a custom encoding.
// BinaryMarshaler / BinaryUnmarshaler type end-to-end. The opportunity
// here is to confirm the typed open path holds together for arbitrary
// N implementations beyond the built-in primitives.
func TestTxn_V2Replay_BinaryMarshaler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewBinaryMarshalerCodec[textKey, *textKey]()
	g := lpg.New[textKey, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec[textKey, int64](g, w, codec)
	a := textKey{prefix: "node", n: 1}
	b := textKey{prefix: "node", n: 2}
	tx := store.Begin()
	if err := tx.AddEdge(a, b); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetEdgeLabel(a, b, "FRIENDS"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := OpenWithCodec[textKey, int64](dir, codec)
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	if res.WALOps != 2 {
		t.Fatalf("WALOps = %d, want 2", res.WALOps)
	}
	if !res.Graph.AdjList().HasEdge(a, b) {
		t.Fatal("a->b edge missing after recovery")
	}
	if !res.Graph.HasEdgeLabel(a, b, "FRIENDS") {
		t.Fatal("a->b FRIENDS missing after recovery")
	}
}

// textKey is a custom node identifier used to exercise the
// BinaryMarshaler-backed codec across the txn + recovery boundary.
type textKey struct {
	prefix string
	n      uint64
}

func (k *textKey) MarshalBinary() ([]byte, error) {
	out := make([]byte, 4+len(k.prefix)+8)
	binary.LittleEndian.PutUint32(out, uint32(len(k.prefix)))
	copy(out[4:], k.prefix)
	binary.LittleEndian.PutUint64(out[4+len(k.prefix):], k.n)
	return out, nil
}

func (k *textKey) UnmarshalBinary(data []byte) error {
	if len(data) < 4 {
		return errCodecShort
	}
	n := binary.LittleEndian.Uint32(data)
	if uint64(len(data)-4) < uint64(n)+8 {
		return errCodecShort
	}
	k.prefix = string(data[4 : 4+n])
	k.n = binary.LittleEndian.Uint64(data[4+n:])
	return nil
}

// errCodecShort is a deliberate test-only error so the
// UnmarshalBinary implementation does not pull a heavyweight package
// into the recovery test surface.
var errCodecShort = errCodec("recovery test: short payload")

type errCodec string

func (e errCodec) Error() string { return string(e) }
