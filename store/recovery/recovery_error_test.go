package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestDecode_ShortPayloads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "empty payload", payload: nil},
		{name: "only kind byte", payload: []byte{byte(txn.OpAddEdge)}},
		{
			name:    "kind plus partial src length prefix",
			payload: []byte{byte(txn.OpAddEdge), 0x01},
		},
		{
			name:    "kind plus src length but missing src bytes",
			payload: []byte{byte(txn.OpAddEdge), 0x04, 0x00},
		},
		{
			name:    "kind plus src ok but missing dst length prefix",
			payload: []byte{byte(txn.OpAddEdge), 0x03, 0x00, 'a', 'b', 'c'},
		},
		{
			name:    "kind plus src plus dst length but missing dst bytes",
			payload: []byte{byte(txn.OpAddEdge), 0x03, 0x00, 'a', 'b', 'c', 0x04, 0x00},
		},
		{
			name: "kind plus src/dst ok but missing label length prefix",
			payload: []byte{
				byte(txn.OpSetEdgeLabel),
				0x03, 0x00, 'a', 'b', 'c',
				0x03, 0x00, 'x', 'y', 'z',
			},
		},
		{
			name: "label length declared but payload truncated",
			payload: []byte{
				byte(txn.OpSetEdgeLabel),
				0x03, 0x00, 'a', 'b', 'c',
				0x03, 0x00, 'x', 'y', 'z',
				0x05, 0x00, // label length = 5 but no bytes follow
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.payload); err == nil {
				t.Fatalf("Decode(%q) returned no error", tc.name)
			}
		})
	}
}

func TestOpen_EmptyDirectoryReturnsEmptyResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open empty dir = %v, want nil", err)
	}
	if res.SnapshotHit {
		t.Fatalf("SnapshotHit on empty dir should be false")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps on empty dir = %d, want 0", res.WALOps)
	}
	if res.Graph == nil {
		t.Fatalf("Graph must be non-nil even on empty dir")
	}
}

func TestOpen_SnapshotPresentNoWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Lay down a valid empty snapshot under dir/snapshot.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	a := g.AdjList()
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if err := snapshot.WriteSnapshotCSR(filepath.Join(dir, "snapshot"), c); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit should be true after writing a snapshot")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (no WAL)", res.WALOps)
	}
}

func TestOpen_CorruptedSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snapshot")
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Plant a manifest.json that does not parse as JSON.
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte("{bogus"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}); err == nil {
		t.Fatalf("Open with corrupted snapshot manifest should error")
	}
}

// TestOpen_UnknownRecordVersionStopsHard appends a CRC-valid WAL frame
// whose transaction-record version byte is 0x01 — a legacy/unknown record
// version that Decode rejects with ErrUnsupportedRecordVersion. Such a
// frame is genuine corruption, not a crash-truncated tail, so the new
// fail-stop contract (task #1289) surfaces it as the function error rather
// than swallowing it into Result.TailErr while Open returns nil. The replay
// still applies zero ops and does not panic.
//
// Before #1289 this test asserted Open returned nil; the per-op decode
// failure surfaced only via Result.TailErr, which no production caller
// inspected — exactly the fail-silent path #1289 closes.
func TestOpen_UnknownRecordVersionStopsHard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// 0x01 == OpAddEdge as a leading byte: not OpRecordV2 (0xFE) or
	// OpRecordV3 (0xFD), so Decode rejects it with ErrUnsupportedRecordVersion.
	if err := w.Append([]byte{byte(txn.OpAddEdge)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err == nil {
		t.Fatal("Open with an unknown record version returned nil; genuine corruption must be surfaced")
	}
	if !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrUnsupportedRecordVersion)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true for an unknown record version, want false")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (decode failed on first op)", res.WALOps)
	}
}

func TestOpen_PreCancelledContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := OpenCtx[string, int64](ctx, dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenCtx pre-cancelled = %v, want context.Canceled", err)
	}
}

func TestOpen_NonExistentWALPathBubblesError(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	// Plant a regular file named "wal" inside dir, then revoke write+x
	// from the parent so that the wal package cannot open it for read.
	// On macOS/Linux a directory with mode 000 forbids open(file)
	// inside it.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(dir, "wal")
	if err := os.WriteFile(walPath, []byte{}, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // test cleanup restores access
	if _, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}); err == nil {
		t.Fatalf("Open with unreadable WAL should error")
	}
}

// TestOpen_ContextCancelledMidReplay drives a deadline that
// expires while we are still replaying so we cross the per-4096-frame
// ctx.Err() checkpoint inside the recovery core.
func TestOpen_ContextCancelledMidReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// Append enough valid frames that the >=4096 checkpoint can fire.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())
	for i := 0; i < 4500; i++ {
		tx := store.Begin()
		_ = tx.SetNodeLabel("alice", "Person")
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Use a context that is already past its deadline so the very
	// first ctx.Err() after the snapshot probe trips. We don't strictly
	// need to land "mid-replay" — any non-nil ctx.Err triggers the
	// abort path inside the recovery core.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := OpenCtx[string, int64](ctx, dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}); err == nil {
		t.Fatalf("OpenCtx with expired deadline should error")
	}
}
