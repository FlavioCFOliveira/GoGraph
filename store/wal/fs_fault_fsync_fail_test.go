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

// TestWALWriter_StickyErrorAfterFsyncFailure is the gate test for
// the phantom-commit fix (task #1333): the first fsync failure must
// permanently poison the [wal.Writer].
//
// Without the poison, the writer stays fully usable after a failed
// Sync: the failed transaction's flushed frames (and commit marker)
// remain physically in the file, and the next transaction's
// successful fsync makes them durable — recovery then replays a
// commit whose Sync never acknowledged. The poison closes that hole:
// after the first failure every Append and Sync returns the original
// error, and the un-synced suffix is discarded.
func TestWALWriter_StickyErrorAfterFsyncFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "sticky.wal")

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
	tx3 := bytes.Repeat([]byte{0xC3}, 32)

	// tx1: append + commit fsync succeed (Sync #1 is honoured).
	if err := w.Append(tx1); err != nil {
		t.Fatalf("Append(tx1): %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync(tx1): %v", err)
	}

	// tx2: Sync #2 fails — the commit must not be acknowledged.
	if err := w.Append(tx2); err != nil {
		t.Fatalf("Append(tx2): %v", err)
	}
	if err := w.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Sync(tx2) = %v; want testfs.ErrSyncFailed", err)
	}

	// The writer must now be poisoned: a subsequent Append is
	// rejected with the original sync error instead of buffering.
	if err := w.Append(tx3); err == nil {
		t.Fatal("Append after failed Sync = nil; want sticky error (writer must be poisoned)")
	} else if !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Append after failed Sync = %v; want the sticky testfs.ErrSyncFailed", err)
	}
	// ... and so is a subsequent Sync.
	if err := w.Sync(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Sync after failed Sync = %v; want the sticky testfs.ErrSyncFailed", err)
	}
	// The rejected frame must not be counted as appended.
	if got := w.Stats().Frames; got != 2 {
		t.Errorf("Stats().Frames = %d, want 2 (tx3 must have been rejected)", got)
	}

	// Close on a poisoned writer surfaces the sticky error (the
	// shutdown is not clean) and must not fsync the discarded suffix.
	if err := w.Close(); !errors.Is(err, testfs.ErrSyncFailed) {
		t.Fatalf("Close = %v; want the sticky testfs.ErrSyncFailed", err)
	}

	// Recovery: only the acknowledged tx1 may be present; tx2 must be
	// absent even though its frames were flushed before the failure.
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
