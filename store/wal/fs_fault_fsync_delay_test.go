package wal_test

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"gograph/internal/testfs"
	"gograph/store/wal"
)

// TestWALFault_FsyncDelay verifies three properties of the FsyncDelay
// fault mode:
//
//  1. A Sync call on a delay-injected writer takes at least the
//     configured delay duration.
//  2. Frames written before Sync are intact and fully readable after
//     Sync completes (delay does not corrupt data).
//  3. Group-commit semantics are preserved: five frames appended with
//     a single Sync are all visible to a subsequent reader.
func TestWALFault_FsyncDelay(t *testing.T) {
	t.Parallel()

	const delay = 50 * time.Millisecond

	// --- sub-test 1: Sync duration ---
	t.Run("sync_takes_at_least_delay", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		walPath := filepath.Join(dir, "delay.wal")

		ff, err := testfs.New(walPath, testfs.Faults{FsyncDelay: delay})
		if err != nil {
			t.Fatalf("testfs.New: %v", err)
		}

		w, err := wal.OpenWith(ff)
		if err != nil {
			_ = ff.Close()
			t.Fatalf("wal.OpenWith: %v", err)
		}
		defer func() { _ = w.Close() }()

		payload := bytes.Repeat([]byte{0xDD}, 40)
		if err := w.Append(payload); err != nil {
			t.Fatalf("Append: %v", err)
		}

		start := time.Now()
		if err := w.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		elapsed := time.Since(start)

		if elapsed < delay {
			t.Errorf("Sync took %v, want >= %v (FsyncDelay not applied)", elapsed, delay)
		}
	})

	// --- sub-test 2: data correctness after delay ---
	t.Run("data_correct_after_delay", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		walPath := filepath.Join(dir, "delay_correct.wal")

		ff, err := testfs.New(walPath, testfs.Faults{FsyncDelay: delay})
		if err != nil {
			t.Fatalf("testfs.New: %v", err)
		}

		w, err := wal.OpenWith(ff)
		if err != nil {
			_ = ff.Close()
			t.Fatalf("wal.OpenWith: %v", err)
		}

		payloads := [][]byte{
			bytes.Repeat([]byte{0x01}, 30),
			bytes.Repeat([]byte{0x02}, 30),
			bytes.Repeat([]byte{0x03}, 30),
		}
		for i, p := range payloads {
			if err := w.Append(p); err != nil {
				t.Fatalf("Append(%d): %v", i, err)
			}
		}
		if err := w.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

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
		if len(decoded) != len(payloads) {
			t.Fatalf("decoded %d frame(s), want %d", len(decoded), len(payloads))
		}
		for i, want := range payloads {
			if !bytes.Equal(decoded[i].Payload, want) {
				t.Errorf("frame %d payload mismatch", i)
			}
		}
	})

	// --- sub-test 3: group commit — 5 appends, 1 sync ---
	t.Run("group_commit_5_frames_1_sync", func(t *testing.T) {
		t.Parallel()

		const nFrames = 5
		dir := t.TempDir()
		walPath := filepath.Join(dir, "group_commit.wal")

		ff, err := testfs.New(walPath, testfs.Faults{FsyncDelay: delay})
		if err != nil {
			t.Fatalf("testfs.New: %v", err)
		}

		w, err := wal.OpenWith(ff)
		if err != nil {
			_ = ff.Close()
			t.Fatalf("wal.OpenWith: %v", err)
		}

		var want [][]byte
		for i := 0; i < nFrames; i++ {
			p := bytes.Repeat([]byte{byte(i + 1)}, 20)
			if err := w.Append(p); err != nil {
				t.Fatalf("Append(%d): %v", i, err)
			}
			want = append(want, p)
		}
		// Single Sync commits all 5 frames.
		if err := w.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

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
		if len(decoded) != nFrames {
			t.Fatalf("decoded %d frame(s), want %d", len(decoded), nFrames)
		}
		for i, p := range want {
			if !bytes.Equal(decoded[i].Payload, p) {
				t.Errorf("frame %d payload mismatch after group-commit", i)
			}
		}
	})
}
