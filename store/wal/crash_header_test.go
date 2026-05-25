package wal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWAL_CrashMidFrameHeader simulates a crash that interrupted the write of
// a frame header: 5 complete frames are durable, then 7 bytes (< HeaderSize=14)
// of a new frame's header were written before the process died.
//
// Recovery must:
//   - Return exactly 5 complete frames.
//   - Report ErrTornFrame via TailError().
func TestWAL_CrashMidFrameHeader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "complete.wal")

	// Write 5 complete frames and sync.
	w, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := w.Append([]byte("header-crash-frame")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read the durable bytes.
	good, err := os.ReadFile(src) //nolint:gosec // t.TempDir-rooted
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Append 7 bytes — a partial header (HeaderSize = 14 bytes).
	// These 7 bytes simulate a torn header write: magic bytes only, no
	// version/length/crc fields, no payload.
	partialHeader := []byte{'G', 'G', 'W', 'A', 0x01, 0x00, 0x00} // 7 bytes
	augmented := make([]byte, len(good)+len(partialHeader))
	copy(augmented, good)
	copy(augmented[len(good):], partialHeader)

	dst := filepath.Join(dir, "torn_header.wal")
	if err := os.WriteFile(dst, augmented, 0o600); err != nil { //nolint:gosec // testdata
		t.Fatalf("WriteFile: %v", err)
	}

	// Open and replay.
	r, err := OpenReader(dst)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	count := 0
	if err := r.Replay(func(_ Frame) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if count != 5 {
		t.Fatalf("decoded %d frames, want 5", count)
	}

	tailErr := r.TailError()
	if !errors.Is(tailErr, ErrTornFrame) {
		t.Fatalf("TailError = %v, want ErrTornFrame", tailErr)
	}
}
