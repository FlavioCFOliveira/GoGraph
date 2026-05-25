package wal_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"gograph/internal/testfs"
	"gograph/store/wal"
)

// TestWALFault_ENOSPC verifies that ENOSPC on every Write is surfaced
// as a non-nil error from [wal.Writer.Append] or [wal.Writer.Sync].
// Because no complete frame is written, the reader must decode zero
// frames and TailError must be nil (clean EOF on an empty file) or a
// torn-frame error if a partial header reached disk (which cannot
// happen with ReturnENOSPC because Write returns 0 bytes every time).
func TestWALFault_ENOSPC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "enospc.wal")

	ff, err := testfs.New(walPath, testfs.Faults{ReturnENOSPC: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}

	// OpenWith seeks to end (no Write yet) — must succeed.
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	// Append triggers the ENOSPC fault on the very first bufio flush or
	// on the explicit Sync that follows. At least one of the two must
	// return an error.
	payload := bytes.Repeat([]byte{0xCC}, 40)
	appendErr := w.Append(payload)
	syncErr := w.Sync()
	_ = w.Close()

	if appendErr == nil && syncErr == nil {
		t.Fatal("Append+Sync on ENOSPC fault file: both returned nil; want at least one error")
	}

	// The error that did fire should wrap ENOSPC or be non-nil due to
	// the bufio partial-write pathway.
	if appendErr != nil && !testfs.IsENOSPC(appendErr) {
		// bufio may wrap as io.ErrShortWrite; accept any non-nil error.
		if !errors.Is(appendErr, testfs.ErrPartialWrite) {
			t.Logf("Append err: %v (not ENOSPC nor ErrPartialWrite; accepting non-nil)", appendErr)
		}
	}
	if syncErr != nil && !testfs.IsENOSPC(syncErr) {
		t.Logf("Sync err: %v (not ENOSPC; accepting non-nil)", syncErr)
	}

	// Because ReturnENOSPC returns 0 bytes on every Write, no frame
	// data reaches disk. The file should be empty or contain at most a
	// partial bufio-internal flush — in either case the reader should
	// decode 0 complete frames.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		// An empty or tiny file is acceptable: OpenReader may return an
		// error if the file size is below one header. Accept that.
		t.Logf("OpenReader after ENOSPC: %v (accepted — file may be empty)", err)
		return
	}
	defer func() { _ = r.Close() }()

	var decoded []wal.Frame
	for f := range r.Frames() {
		decoded = append(decoded, f)
	}
	if len(decoded) != 0 {
		t.Errorf("decoded %d frame(s) after ENOSPC; want 0", len(decoded))
	}
}

// TestWALFault_ENOSPCAfterKFrames verifies that a partial write
// budget (FailWritesAfterBytes) that expires mid-frame causes the
// reader to see exactly the complete frames before the fault, with a
// non-nil TailError for the torn trailing frame.
//
// Frame geometry with 40-byte payloads:
//
//	HeaderSize (14) + 40 = 54 bytes per frame
//
// With FailWritesAfterBytes=100:
//   - Frame 1 occupies bytes 0–53 (54 bytes); fits within 100.
//   - Frame 2 starts at byte 54; the budget is exhausted at byte 100,
//     cutting the second frame 46 bytes in (header + 32 bytes of
//     payload, CRC check will fail).
func TestWALFault_ENOSPCAfterKFrames(t *testing.T) {
	t.Parallel()

	const (
		payloadSize = 40
		// HeaderSize = 14 (magic 4 + version 2 + length 4 + crc32c 4)
		frameSize = 14 + payloadSize // 54
		budget    = 100              // > 1 frame, < 2 frames
	)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "enospc_k.wal")

	ff, err := testfs.New(walPath, testfs.Faults{FailWritesAfterBytes: budget})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}

	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	payload1 := bytes.Repeat([]byte{0xAA}, payloadSize)
	if err := w.Append(payload1); err != nil {
		t.Fatalf("Append(frame1): unexpected error: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after frame1: unexpected error: %v", err)
	}

	payload2 := bytes.Repeat([]byte{0xBB}, payloadSize)
	_ = w.Append(payload2) // expected to fail at or before Sync
	_ = w.Sync()           // tolerate error
	_ = w.Close()

	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	var decoded []wal.Frame
	for f := range r.Frames() {
		decoded = append(decoded, f)
	}

	// Exactly one complete frame must have been decoded.
	if len(decoded) != 1 {
		t.Errorf("decoded %d frame(s), want 1 (budget=%d, frameSize=%d)", len(decoded), budget, frameSize)
	}
	if len(decoded) == 1 && !bytes.Equal(decoded[0].Payload, payload1) {
		t.Errorf("frame 1 payload mismatch: got %d bytes, want %d", len(decoded[0].Payload), payloadSize)
	}

	// TailError must be non-nil: the partial second frame is torn.
	tailErr := r.TailError()
	if tailErr == nil {
		t.Error("TailError() = nil; want ErrTornFrame/ErrBadMagic/ErrCRCMismatch for the torn second frame")
	} else if !errors.Is(tailErr, wal.ErrTornFrame) &&
		!errors.Is(tailErr, wal.ErrBadMagic) &&
		!errors.Is(tailErr, wal.ErrCRCMismatch) {
		t.Errorf("TailError() = %v; want one of ErrTornFrame/ErrBadMagic/ErrCRCMismatch", tailErr)
	}
}
