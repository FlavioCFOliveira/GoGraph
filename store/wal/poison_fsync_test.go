package wal_test

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// syncCountFile wraps a testfs.FaultFile and counts Sync calls so
// tests can assert that poison() issues a fsync after its Truncate.
//
// It is not safe for concurrent use; all tests that use it are serial
// relative to the Writer under test (the Writer serialises on mu).
type syncCountFile struct {
	inner     *testfs.FaultFile
	syncs     atomic.Int64
	truncates atomic.Int64
}

func (s *syncCountFile) Write(p []byte) (int, error) { return s.inner.Write(p) }
func (s *syncCountFile) Read(p []byte) (int, error)  { return s.inner.Read(p) }
func (s *syncCountFile) Seek(off int64, whence int) (int64, error) {
	return s.inner.Seek(off, whence)
}
func (s *syncCountFile) Sync() error {
	s.syncs.Add(1)
	return s.inner.Sync()
}
func (s *syncCountFile) Truncate(size int64) error {
	s.truncates.Add(1)
	return s.inner.Truncate(size)
}
func (s *syncCountFile) Close() error { return s.inner.Close() }

// TestPoison_TruncationIsDurable verifies that poison() issues an
// fsync after the Truncate call so the reduced inode size is durable
// across a host crash.
//
// Without the post-truncation fsync a crash between the ftruncate(2)
// syscall and the kernel's inode-metadata writeback could restore the
// file to its pre-truncation size on the next mount, making a failed
// transaction's frames visible to recovery — a Durability violation.
func TestPoison_TruncationIsDurable(t *testing.T) {
	t.Parallel()

	// Build a WAL with FailSyncAfter:1 so:
	//  - tx1 commits durably (first Sync succeeds).
	//  - tx2's Sync fails and triggers poison().
	//
	// We count how many times Sync is called on the underlying file
	// to confirm that poison() issued at least one more Sync (the
	// post-truncation fsync) in addition to the failed commit Sync.
	ff, err := testfs.New(t.TempDir()+"/poison.wal", testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	spy := &syncCountFile{inner: ff}

	w, err := wal.OpenWith(spy)
	if err != nil {
		_ = spy.Close()
		t.Fatalf("OpenWith: %v", err)
	}

	tx1 := bytes.Repeat([]byte{0xAA}, 16)
	tx2 := bytes.Repeat([]byte{0xBB}, 16)

	if err := w.Append(tx1); err != nil {
		t.Fatalf("Append tx1: %v", err)
	}
	if err := w.Sync(); err != nil { // this Sync succeeds
		t.Fatalf("Sync tx1: %v", err)
	}
	syncsBefore := spy.syncs.Load()

	if err := w.Append(tx2); err != nil {
		t.Fatalf("Append tx2: %v", err)
	}
	syncErr := w.Sync() // this Sync fails → triggers poison()
	if syncErr == nil {
		t.Fatal("Sync tx2: expected error, got nil")
	}
	if !errors.Is(syncErr, testfs.ErrSyncFailed) {
		t.Fatalf("Sync tx2: got %v, want ErrSyncFailed", syncErr)
	}

	// poison() must have called Sync at least once after the failed
	// fsync attempt (the post-truncation durability fsync).
	syncsAfterPoison := spy.syncs.Load()
	// Expected calls on spy after syncsBefore:
	//   1 — the failing Sync in SyncCtx
	//   1 — the post-truncation Sync in poison()
	// Total ≥ 2.
	if syncsAfterPoison-syncsBefore < 2 {
		t.Errorf("Sync calls during tx2 commit+poison = %d, want ≥ 2 (failing commit + post-truncation fsync)",
			syncsAfterPoison-syncsBefore)
	}

	// Truncate must also have been called (the actual discard).
	if spy.truncates.Load() == 0 {
		t.Error("Truncate was never called during poison; expected at least one call")
	}

	// Close of a poisoned writer re-attempts the second-chance
	// truncate + fsync; count them too.
	syncsBeforeClose := spy.syncs.Load()
	truncatesBeforeClose := spy.truncates.Load()
	closeErr := w.Close()
	if !errors.Is(closeErr, testfs.ErrSyncFailed) {
		t.Fatalf("Close: got %v, want ErrSyncFailed (sticky poison error)", closeErr)
	}
	if spy.syncs.Load() <= syncsBeforeClose {
		t.Error("Close of poisoned writer did not issue a post-truncation fsync")
	}
	if spy.truncates.Load() <= truncatesBeforeClose {
		t.Error("Close of poisoned writer did not re-attempt Truncate")
	}
}

// TestPoison_FileRemainsAtDurableSize verifies that after poison() the
// underlying file holds only the durably committed bytes (the first
// transaction) and the un-committed suffix is discarded.
func TestPoison_FileRemainsAtDurableSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := dir + "/durable.wal"

	ff, err := testfs.New(walPath, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}

	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("OpenWith: %v", err)
	}

	tx1 := bytes.Repeat([]byte{0xCC}, 32)
	tx2 := bytes.Repeat([]byte{0xDD}, 32)

	if err := w.Append(tx1); err != nil {
		t.Fatalf("Append tx1: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync tx1: %v", err)
	}

	if err := w.Append(tx2); err != nil {
		t.Fatalf("Append tx2: %v", err)
	}
	if syncErr := w.Sync(); syncErr == nil {
		t.Fatal("Sync tx2: expected error, got nil")
	}

	// Close the poisoned writer (best-effort; second-chance discard).
	_ = w.Close()

	// Re-open the WAL file for reading and confirm only tx1 is recoverable.
	r, openErr := wal.OpenReader(walPath)
	if openErr != nil {
		t.Fatalf("OpenReader: %v", openErr)
	}
	defer func() { _ = r.Close() }()

	var frames []wal.Frame
	for f := range r.Frames() {
		frames = append(frames, f)
	}
	if r.TailError() != nil {
		t.Errorf("TailError = %v; want nil", r.TailError())
	}
	if len(frames) != 1 {
		t.Fatalf("recovered %d frame(s), want exactly 1 (tx1 only)", len(frames))
	}
	if !bytes.Equal(frames[0].Payload, tx1) {
		t.Errorf("recovered frame payload mismatch")
	}
}

// TestPoison_ReadersCannotDecodeDiscardedFrames verifies the replay
// invariant: a reader opened after a poison() + Close sees only the
// durably committed prefix, not the discarded suffix.
func TestPoison_ReadersCannotDecodeDiscardedFrames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := dir + "/replay.wal"

	// Write two transactions: the second commit fails.
	ff, err := testfs.New(walPath, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("OpenWith: %v", err)
	}

	committed := []byte("durable")
	uncommitted := []byte("lost")

	if err := w.Append(committed); err != nil {
		t.Fatalf("Append committed: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync committed: %v", err)
	}
	if err := w.Append(uncommitted); err != nil {
		t.Fatalf("Append uncommitted: %v", err)
	}
	_ = w.Sync() // fails → poison()
	_ = w.Close()

	// Open a fresh reader and confirm only "durable" comes back.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	var got [][]byte
	for f := range r.Frames() {
		cp := make([]byte, len(f.Payload))
		copy(cp, f.Payload)
		got = append(got, cp)
	}
	if err := r.TailError(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("TailError = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frames after poison = %d, want 1", len(got))
	}
	if string(got[0]) != "durable" {
		t.Errorf("frame[0] = %q, want %q", got[0], "durable")
	}
}
