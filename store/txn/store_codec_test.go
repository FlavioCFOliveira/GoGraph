package txn

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestNewStoreWithCodec_EmitsV3 asserts that a typed-codec store
// produces v3-tagged frames on the WAL — one per op plus a trailing
// OpCommit marker that makes the transaction atomic on recovery — and
// that the [Store.Codec] accessor returns the codec the caller passed in.
func TestNewStoreWithCodec_EmitsV3(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	codec := NewStringCodec()
	s := NewStoreWithCodec[string, int64](g, w, codec)
	if got := s.Codec(); got == nil {
		t.Fatal("Store.Codec returned nil")
	}
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Inspect the on-disk payload bytes via the reader.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var kinds []byte
	if err := r.Replay(func(f wal.Frame) error {
		if len(f.Payload) < 2 {
			t.Fatalf("payload too short: %d", len(f.Payload))
		}
		if f.Payload[0] != OpRecordV3 {
			t.Fatalf("v3 store emitted byte 0x%02x at offset 0, want 0x%02x", f.Payload[0], OpRecordV3)
		}
		kinds = append(kinds, f.Payload[1])
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	// Two frames: the handle-bearing OpAddEdgeH op (the codec-only
	// AddEdge path mints a stable handle so replay over a snapshot is
	// idempotent) then the OpCommit marker.
	want := []byte{byte(OpAddEdgeH), byte(OpCommit)}
	if len(kinds) != len(want) {
		t.Fatalf("frame count = %d, want %d", len(kinds), len(want))
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("frame %d kind = 0x%02x, want 0x%02x", i, kinds[i], k)
		}
	}
}
