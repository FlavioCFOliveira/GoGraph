package wal

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWAL_CrashMidFsync_Simulation simulates "crash before fsync" by
// truncating the file back to the size it had after the last successful Sync.
//
// Sequence:
//  1. Write 10 frames, Sync, Close — durable state is 10 frames.
//  2. Re-open, write 5 more frames, Close WITHOUT Sync — simulates in-process
//     data that was never flushed to the OS.
//  3. Truncate the file to the size recorded after step 1 — simulates the OS
//     discarding all writes that had not been fsynced.
//  4. Open via OpenReader and Replay.
//  5. Assert exactly 10 frames are recovered and no error is returned.
func TestWAL_CrashMidFsync_Simulation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "fsync_crash.wal")

	// Step 1: write 10 frames and sync.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := w.Append([]byte("durable-frame")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Record the durable file size.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	durableSize := info.Size()

	// Step 2: append 5 more frames without syncing — simulate in-buffer data
	// that would be lost on a crash. We intentionally do not call Sync before
	// Close so that the bufio buffer may or may not flush; we correct for that
	// in step 3 regardless.
	w2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := w2.Append([]byte("unsynced-frame")); err != nil {
			t.Fatalf("Append (unsynced): %v", err)
		}
	}
	// Close flushes the bufio buffer but that is fine — step 3 truncates it.
	_ = w2.Close()

	// Step 3: truncate back to the durable state.
	if err := os.Truncate(path, durableSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	// Step 4 + 5: replay and assert exactly 10 frames.
	r, err := OpenReader(path)
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
	if count != 10 {
		t.Fatalf("recovered %d frames, want 10", count)
	}
	if r.TailError() != nil {
		t.Fatalf("TailError = %v, want nil", r.TailError())
	}
}
