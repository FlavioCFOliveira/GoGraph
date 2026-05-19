package txn

import (
	"errors"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// TestNewStoreWithCodec_EmitsV2 asserts that a typed-codec store
// produces v2-tagged frames on the WAL and that the [Store.Codec]
// accessor returns the codec the caller passed in.
func TestNewStoreWithCodec_EmitsV2(t *testing.T) {
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
	var seen int
	if err := r.Replay(func(f wal.Frame) error {
		seen++
		if len(f.Payload) < 2 {
			t.Fatalf("payload too short: %d", len(f.Payload))
		}
		if f.Payload[0] != OpRecordV2 {
			t.Fatalf("v2 store emitted byte 0x%02x at offset 0, want 0x%02x", f.Payload[0], OpRecordV2)
		}
		if f.Payload[1] != byte(OpAddEdge) {
			t.Fatalf("v2 store emitted OpKind 0x%02x, want 0x%02x", f.Payload[1], byte(OpAddEdge))
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if seen != 1 {
		t.Fatalf("frame count = %d, want 1", seen)
	}
}

// TestNewStore_CodecIsLegacy checks that [NewStore] installs the
// legacy fmt codec and that [isLegacyCodec] recognises it.
func TestNewStore_CodecIsLegacy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStore(g, w)
	c := s.Codec()
	if !isLegacyCodec[string](c) {
		t.Fatal("NewStore did not install the legacy codec")
	}
	// The legacy codec's Encode mirrors goFormat output...
	got := c.Encode(nil, "alice")
	if string(got) != "alice" {
		t.Fatalf("legacy Encode = %q, want %q", got, "alice")
	}
	// ...and its Decode always errors.
	if _, _, err := c.Decode([]byte{1, 2, 3}); !errors.Is(err, ErrCodecDecode) {
		t.Fatalf("legacy Decode err = %v, want ErrCodecDecode", err)
	}
}

// TestNewStoreWithCodec_LegacyCodecKeepsLegacyOutput asserts that
// passing the legacy codec into [NewStoreWithCodec] keeps the v1
// on-disk layout — the version tag is NOT emitted because the
// constructor sets the legacy flag.
func TestNewStoreWithCodec_LegacyCodecKeepsLegacyOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec[string, int64](g, w, legacyFmtCodec[string]{})
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.Replay(func(f wal.Frame) error {
		if f.Payload[0] == OpRecordV2 {
			t.Fatalf("legacy codec must not emit v2 tag")
		}
		if f.Payload[0] != byte(OpAddEdge) {
			t.Fatalf("first byte = 0x%02x, want OpAddEdge", f.Payload[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}
