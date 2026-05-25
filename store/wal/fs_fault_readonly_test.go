package wal_test

import (
	"path/filepath"
	"testing"

	"gograph/internal/testfs"
	"gograph/store/wal"
)

// TestWALFault_ReadOnly simulates a read-only medium by configuring
// [testfs.FaultFile] to return ENOSPC on every Write. The test
// verifies that:
//
//   - [wal.OpenWith] succeeds (no Write occurs during open).
//   - [wal.Writer.Append] or [wal.Writer.Sync] returns a non-nil error.
//   - [wal.Writer.Close] does not panic.
func TestWALFault_ReadOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "readonly.wal")

	ff, err := testfs.New(walPath, testfs.Faults{ReturnENOSPC: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}

	// OpenWith must succeed: it only seeks to EOF, no writes yet.
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	// Append + Sync: at least one must surface an error due to ENOSPC.
	appendErr := w.Append([]byte("readonly-payload"))
	syncErr := w.Sync()

	// Close must not panic regardless of the prior errors.
	closeErr := w.Close()
	_ = closeErr

	if appendErr == nil && syncErr == nil {
		t.Error("Append+Sync on ENOSPC writer: both nil; want at least one error")
	}
}
