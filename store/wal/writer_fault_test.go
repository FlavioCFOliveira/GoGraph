package wal_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"gograph/internal/testfs"
	"gograph/store/wal"
)

// TestWriterFault_TornFrameAtByte128 is the AC#2 demo test for
// task #527. It creates a WAL writer backed by a [testfs.FaultFile]
// configured with FailWritesAfterBytes=128 and appends enough frames
// so that the fault fires mid-frame. The test then opens a WAL
// reader and verifies that the reader detects the torn frame.
//
// The WAL frame format:
//
//	| magic 4B | version 2B | length 4B | crc32c 4B | payload NB |
//	HeaderSize = 14 bytes
//
// With a 100-byte payload each frame is 114 bytes. The fault fires
// after 128 bytes total: the first frame (0–113) is written in full;
// the second frame starts at byte 114 and the fault interrupts it
// at byte 128 (after writing 14 bytes — the complete header, but
// with zero bytes of payload, so the CRC check fails or the payload
// read returns ErrTornFrame).
func TestWriterFault_TornFrameAtByte128(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "fault.wal")

	// Open a FaultFile at the WAL path with a 128-byte write budget.
	ff, err := testfs.New(walPath, testfs.Faults{FailWritesAfterBytes: 128})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	// Ownership is transferred to the Writer; do NOT call ff.Close()
	// separately.

	// Wrap the FaultFile in a WAL Writer.
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	// Append a first frame (100-byte payload → 114 bytes on disk).
	payload1 := bytes.Repeat([]byte{0xAA}, 100)
	if err := w.Append(payload1); err != nil {
		t.Fatalf("Append(frame1): unexpected error: %v", err)
	}
	// Sync flushes bufio and fsyncs: the 114 bytes of frame 1 land.
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after frame1: unexpected error: %v", err)
	}

	// Append a second frame — the 128-byte budget is hit partway
	// through. The error from Append or the following Sync is expected.
	payload2 := bytes.Repeat([]byte{0xBB}, 100)
	_ = w.Append(payload2) // may or may not error depending on bufio buffering
	_ = w.Sync()           // tolerate fault error here
	_ = w.Close()          // best-effort close

	// Open a WAL Reader and iterate all frames.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	var decoded []wal.Frame
	for f := range r.Frames() {
		decoded = append(decoded, f)
	}

	// TailError must indicate a torn or corrupted frame (not clean EOF).
	tailErr := r.TailError()
	if tailErr == nil {
		t.Error("TailError() = nil; want ErrTornFrame/ErrBadMagic/ErrCRCMismatch (torn second frame)")
	} else if !errors.Is(tailErr, wal.ErrTornFrame) &&
		!errors.Is(tailErr, wal.ErrBadMagic) &&
		!errors.Is(tailErr, wal.ErrCRCMismatch) {
		t.Errorf("TailError() = %v; want one of ErrTornFrame/ErrBadMagic/ErrCRCMismatch", tailErr)
	}

	// Exactly one complete frame must have been decoded.
	if len(decoded) != 1 {
		t.Errorf("decoded %d frame(s), want 1 (torn second frame must be rejected)", len(decoded))
	}
	if len(decoded) == 1 && !bytes.Equal(decoded[0].Payload, payload1) {
		t.Errorf("frame 1 payload mismatch: got %d bytes, want %d", len(decoded[0].Payload), len(payload1))
	}
}
