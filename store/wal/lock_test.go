package wal_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestWALOpen_ExclusiveLock is the regression gate for the inter-process
// WAL exclusive lock (task #1416).
//
// Without an OS-level exclusive lock, two processes opening the same WAL
// path would interleave frames silently, corrupting the log. Open now
// acquires a flock(2) (Unix) or O_EXCL (other platforms) on a LOCK sentinel
// file in the same directory before any WAL data is touched. The second
// opener must receive [ErrWALLocked] and leave the WAL intact.
func TestWALOpen_ExclusiveLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "test.wal")

	// First opener: must succeed and hold the lock.
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	defer func() { _ = w1.Close() }()

	// Second opener on the same path: must be rejected with ErrWALLocked.
	w2, err := wal.Open(walPath)
	if err == nil {
		_ = w2.Close()
		t.Fatal("Open (second): expected ErrWALLocked, got nil error")
	}
	if !errors.Is(err, wal.ErrWALLocked) {
		t.Fatalf("Open (second): got %v, want errors.Is ErrWALLocked", err)
	}

	// After the first writer is closed its lock must be released, allowing
	// a third opener to succeed.
	if err := w1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	w3, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("Open (third, after close): %v", err)
	}
	if err := w3.Close(); err != nil {
		t.Fatalf("Close (third): %v", err)
	}
}

// TestWALOpen_ExclusiveLock_FreshAndExisting checks both the fresh-file
// and the existing-file Open paths to confirm the lock is acquired before
// any WAL data is read or written in both cases.
func TestWALOpen_ExclusiveLock_FreshAndExisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "existing.wal")

	// Pre-create the WAL file so the existing-file path is exercised.
	w0, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("Open (initial create): %v", err)
	}
	if err := w0.Close(); err != nil {
		t.Fatalf("Close (initial): %v", err)
	}

	// Open the existing file: must succeed and hold the lock.
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("Open (existing): %v", err)
	}

	// Concurrent open on the same existing file: must be rejected.
	w2, err := wal.Open(walPath)
	if err == nil {
		_ = w2.Close()
		_ = w1.Close()
		t.Fatal("Open (concurrent on existing): expected ErrWALLocked, got nil")
	}
	if !errors.Is(err, wal.ErrWALLocked) {
		_ = w1.Close()
		t.Fatalf("Open (concurrent on existing): got %v, want ErrWALLocked", err)
	}

	if err := w1.Close(); err != nil {
		t.Fatalf("Close (existing): %v", err)
	}
}
