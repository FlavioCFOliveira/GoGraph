package wal_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestWriterFault_TornFrameAtByte128 is the AC#2 demo test for task
// #527, updated for the sync-failure poison contract of task #1333.
// It creates a WAL writer backed by a [testfs.FaultFile] configured
// with FailWritesAfterBytes=128 and appends enough frames so that
// the fault fires mid-frame.
//
// The WAL frame format:
//
//	| magic 4B | version 2B | length 4B | crc32c 4B | payload NB |
//	HeaderSize = 14 bytes
//
// With a 100-byte payload each frame is 114 bytes. The fault fires
// after 128 bytes total: the first frame (0–113) is written in full;
// the second frame's flush is interrupted at byte 128 (after 14
// bytes — a complete header with zero payload bytes). The failed
// Sync poisons the writer and physically discards the un-synced
// suffix, so the reader must recover exactly frame 1 with a clean
// tail, and the writer must reject every subsequent Append.
// Reader-side detection of torn frames that survive on disk (the
// crash case, where no writer is alive to scrub them) is covered by
// crash_header_test.go, crash_payload_test.go and
// torn_offsets_test.go.
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
	// through its flush, so the following Sync must fail.
	payload2 := bytes.Repeat([]byte{0xBB}, 100)
	_ = w.Append(payload2) // buffered: 214 bytes fit in bufio, no error yet
	if err := w.Sync(); err == nil {
		t.Fatal("Sync after frame2 = nil; want a write-budget error")
	}
	// The failed Sync poisons the writer: further appends are rejected.
	if err := w.Append(payload2); err == nil {
		t.Error("Append after failed Sync = nil; want sticky error (writer must be poisoned)")
	}
	_ = w.Close() // unclean shutdown: surfaces the sticky error

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

	// The poison discarded the torn 14-byte suffix: the tail is clean.
	if tailErr := r.TailError(); tailErr != nil {
		t.Errorf("TailError() = %v; want nil (poison must discard the torn suffix)", tailErr)
	}

	// Exactly one complete frame must have been decoded.
	if len(decoded) != 1 {
		t.Errorf("decoded %d frame(s), want 1 (un-synced second frame must be discarded)", len(decoded))
	}
	if len(decoded) == 1 && !bytes.Equal(decoded[0].Payload, payload1) {
		t.Errorf("frame 1 payload mismatch: got %d bytes, want %d", len(decoded[0].Payload), len(payload1))
	}
}
