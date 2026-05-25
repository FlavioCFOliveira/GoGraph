package wal

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestWAL_GroupCommit_ConcurrentWriters spawns 64 goroutines each appending
// 100 frames to a shared WAL Writer, then performs a single group-commit Sync.
// After all goroutines finish, the file is replayed via OpenReader and the
// total frame count is verified.
//
// Assertions:
//   - Total frames replayed == 64 * 100 = 6400.
//   - No Append errors occurred (checked via shared atomic counter).
//   - All frames have valid CRCs (implicit: Replay would return ErrCRCMismatch
//     if any frame were corrupt).
//   - go test -race passes (race detector is the primary correctness gate).
func TestWAL_GroupCommit_ConcurrentWriters(t *testing.T) {
	t.Parallel()

	const goroutines = 64
	const framesPerGoroutine = 100
	const totalFrames = goroutines * framesPerGoroutine

	dir := t.TempDir()
	path := filepath.Join(dir, "group_commit.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var errCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < framesPerGoroutine; i++ {
				payload := []byte(fmt.Sprintf("g%03d-f%03d", g, i))
				if err := w.Append(payload); err != nil {
					errCount.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()

	if n := errCount.Load(); n != 0 {
		t.Fatalf("%d Append calls returned errors", n)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Replay and count all frames.
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

	if count != totalFrames {
		t.Fatalf("replayed %d frames, want %d", count, totalFrames)
	}
	if r.TailError() != nil {
		t.Fatalf("TailError = %v, want nil", r.TailError())
	}
}
