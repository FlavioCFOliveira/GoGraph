package snapshot

// full_codec_test.go — coverage for WriteSnapshotFullWithMapperCodec,
// writeMapperWithCodec, and readVerifiedMapperBytes, all of which were at 0%.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// TestWriteSnapshotFullWithMapperCodec_RoundTrip is the primary success path:
// write a string-keyed graph via the codec-aware writer, read it back via
// LoadSnapshotFull, and confirm the mapper readback holds all original pairs.
func TestWriteSnapshotFullWithMapperCodec_RoundTrip(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")

	codec := txn.NewStringCodec()
	if err := WriteSnapshotFullWithMapperCodec(dir, c, g, codec); err != nil {
		t.Fatalf("WriteSnapshotFullWithMapperCodec: %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	// String-keyed snapshot written via the codec-aware path must still
	// produce a manifest at ManifestVersion (v3) with a mapper.bin entry.
	if loaded.Manifest.Version != ManifestVersion {
		t.Fatalf("manifest version = %d, want %d", loaded.Manifest.Version, ManifestVersion)
	}
	// The mapper readback must carry both interned nodes.
	if len(loaded.Mapper.Pairs) < 2 {
		t.Fatalf("mapper pairs = %d, want ≥ 2", len(loaded.Mapper.Pairs))
	}
}

// TestWriteSnapshotFullWithMapperCodec_NonString exercises the non-string
// code path (writeMapperWithCodec) by using an int64-keyed graph. The
// resulting snapshot must be loadable and must carry a v2 mapper.bin with
// at least one RawPair.
func TestWriteSnapshotFullWithMapperCodec_NonString(t *testing.T) {
	t.Parallel()
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	if err := g.AddEdge(1, 2, 0.5); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")

	codec := txn.NewInt64Codec()
	if err := WriteSnapshotFullWithMapperCodec(dir, c, g, codec); err != nil {
		t.Fatalf("WriteSnapshotFullWithMapperCodec (int64): %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull (int64 snap): %v", err)
	}
	// Non-string keys produce a v3 manifest via the codec path.
	if loaded.Manifest.Version != ManifestVersion {
		t.Fatalf("manifest version = %d, want %d", loaded.Manifest.Version, ManifestVersion)
	}
	// The mapper readback for a non-string key uses RawPairs.
	if len(loaded.Mapper.RawPairs) < 2 {
		t.Fatalf("mapper RawPairs = %d, want ≥ 2 (int64 keys)", len(loaded.Mapper.RawPairs))
	}
}

// TestWriteSnapshotFullWithMapperCodec_NilCodecErrors confirms the nil-codec
// guard path: a nil codec must return an error without touching the filesystem.
func TestWriteSnapshotFullWithMapperCodec_NilCodecErrors(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("x"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")

	err := WriteSnapshotFullWithMapperCodec[string, int64](dir, c, g, nil)
	if err == nil {
		t.Fatal("WriteSnapshotFullWithMapperCodec with nil codec must return an error")
	}
	// The snapshot directory must NOT have been published.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot dir must not exist after nil-codec error, stat=%v", statErr)
	}
}

// TestWriteSnapshotFullWithMapperCodec_CancelledCtx exercises the context-
// cancel path at the earliest checkpoint inside writeSnapshotFullCore.
func TestWriteSnapshotFullWithMapperCodec_CancelledCtx(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	sentinel := errors.New("cancel before any write")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  0, // first Err() returns sentinel immediately
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullWithMapperCodecCtx(ctx, dir, c, g, txn.NewStringCodec()); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotFullWithMapperCodecCtx pre-cancel = %v, want %v", err, sentinel)
	}
}

// TestWriteSnapshotFullWithMapperCodec_ReadOnly exercises the OS-level
// failure path: a read-only parent directory prevents the staging directory
// from being created.
func TestWriteSnapshotFullWithMapperCodec_ReadOnly(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("running as root: read-only permission tests are unreliable")
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("x"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil { //nolint:gosec // intentionally read-only
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) }) //nolint:gosec // restore for cleanup

	dir := filepath.Join(readOnly, "snap")
	if err := WriteSnapshotFullWithMapperCodec(dir, c, g, txn.NewStringCodec()); err == nil {
		t.Fatal("WriteSnapshotFullWithMapperCodec into read-only parent must error")
	}
}
