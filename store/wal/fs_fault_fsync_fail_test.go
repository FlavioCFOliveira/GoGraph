package wal_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestWALFault_FsyncFailure_CommitNotAcknowledged is the gate test
// for the fsync-failure durability branch (Writer.SyncCtx syncFailed
// path): a commit whose fsync fails must NOT be acknowledged, and a
// subsequent recovery must observe only the durably-committed
// prefix.
//
// FailSyncAfter:1 lets tx1's commit fsync succeed and fails tx2's.
// On the failure, testfs discards tx2's un-synced bytes (modelling a
// kernel that drops dirty pages after a failed fsync), so reopening
// the WAL file is equivalent to recovering after a crash that
// follows the failed commit.
func TestWALFault_FsyncFailure_CommitNotAcknowledged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "fsync_fail.wal")

	ff, err := testfs.New(walPath, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}

	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	tx1 := bytes.Repeat([]byte{0xA1}, 32)
	tx2 := bytes.Repeat([]byte{0xB2}, 32)

	// tx1: append + commit fsync must succeed (first Sync is honoured).
	if err := w.Append(tx1); err != nil {
		t.Fatalf("Append(tx1): %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync(tx1): %v", err)
	}

	// tx2: the commit fsync fails — the commit must NOT be acknowledged.
	if err := w.Append(tx2); err != nil {
		t.Fatalf("Append(tx2): %v", err)
	}
	err = w.Sync()
	if err == nil {
		t.Fatal("Sync(tx2) = nil; want error (failed commit must not be acknowledged)")
	}
	if !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Sync(tx2) = %v; want testfs.ErrSyncFailed", err)
	}
	if got := w.Stats().SyncFailed; got != 1 {
		t.Errorf("Stats().SyncFailed = %d, want 1", got)
	}

	// Close flushes + fsyncs once more; the fault keeps firing, so an
	// error is expected here and the underlying file is still released
	// (Writer.Close closes best-effort on its error path).
	if err := w.Close(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Close: err=%v; want ErrSyncFailed (fault still armed)", err)
	}

	// Recovery: only the acknowledged commit may be present.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	var decoded []wal.Frame
	for f := range r.Frames() {
		decoded = append(decoded, f)
	}
	if r.TailError() != nil {
		t.Errorf("TailError() = %v; want nil", r.TailError())
	}
	if len(decoded) != 1 {
		t.Fatalf("recovered %d frame(s), want 1 (only the acknowledged tx1)", len(decoded))
	}
	if !bytes.Equal(decoded[0].Payload, tx1) {
		t.Errorf("recovered payload = % 02x, want tx1", decoded[0].Payload)
	}
}
