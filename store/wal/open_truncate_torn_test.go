package wal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpen_TruncatesBenignTornTail verifies that [Open] discards a
// benign torn trailing frame before appending: the file is truncated
// back to the last durable frame boundary, and frames appended after
// the reopen are reachable by a subsequent reader (they would
// otherwise sit unreachable behind the torn junk — the durability
// hole closed by discardTornTail).
func TestOpen_TruncatesBenignTornTail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "torn.wal")

	// Write two durable frames.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, p := range []string{"frame-0", "frame-1"} {
		if err := w.Append([]byte(p)); err != nil {
			t.Fatalf("Append(%s): %v", p, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	durableSize := info.Size()

	// Tear the tail: 10 bytes is less than HeaderSize (14), so the
	// trailing bytes are an unfinished frame header.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write(make([]byte, 10)); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close junk writer: %v", err)
	}

	// Reopen for append: the torn tail must be discarded.
	w2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat (after reopen): %v", err)
	}
	if info.Size() != durableSize {
		t.Errorf("file size after reopen = %d, want %d (torn tail kept)", info.Size(), durableSize)
	}
	if err := w2.Append([]byte("frame-2")); err != nil {
		t.Fatalf("Append(frame-2): %v", err)
	}
	if err := w2.Sync(); err != nil {
		t.Fatalf("Sync (reopen): %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close (reopen): %v", err)
	}

	// Every frame — including the post-reopen one — must be readable.
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var got []string
	for fr := range r.Frames() {
		got = append(got, string(fr.Payload))
	}
	if r.TailError() != nil {
		t.Fatalf("TailError = %v, want nil", r.TailError())
	}
	want := []string{"frame-0", "frame-1", "frame-2"}
	if len(got) != len(want) {
		t.Fatalf("recovered %d frames %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestOpen_PreservesGenuineCorruption verifies that [Open] does NOT
// truncate when the scan stops at genuine corruption inside an
// already-durable frame (here a CRC mismatch): the bytes are preserved
// byte-for-byte for diagnosis, and fail-stop is the recovery layer's
// responsibility, not Open's.
func TestOpen_PreservesGenuineCorruption(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, p := range []string{"frame-0", "frame-1", "frame-2"} {
		if err := w.Append([]byte(p)); err != nil {
			t.Fatalf("Append(%s): %v", p, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt one payload byte of the middle frame; the frame layout is
	// HeaderSize + 7 payload bytes per frame.
	raw, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	frameSize := HeaderSize + len("frame-0")
	raw[frameSize+HeaderSize] ^= 0xFF // first payload byte of frame-1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	originalSize := int64(len(raw))

	// Reopen: Open must leave the corrupt file intact.
	w2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (corrupt reopen): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != originalSize {
		t.Errorf("file size after reopen = %d, want %d (corruption must be preserved)",
			info.Size(), originalSize)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close (corrupt reopen): %v", err)
	}

	// The reader must still surface the CRC mismatch at frame-1.
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	count := 0
	for range r.Frames() {
		count++
	}
	if count != 1 {
		t.Errorf("readable frames = %d, want 1 (corruption at frame-1)", count)
	}
	if !errors.Is(r.TailError(), ErrCRCMismatch) {
		t.Errorf("TailError = %v, want ErrCRCMismatch", r.TailError())
	}
}
