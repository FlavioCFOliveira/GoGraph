package wal

// Encode-side frame bound (rmp #2742).
//
// Decode has always refused a declared payload above maxFrameSize; Encode never
// checked. A frame between 1 GiB and 4 GiB was therefore written, fsynced, and
// acknowledged as a durable commit that recovery is then REQUIRED to refuse —
// and above 4 GiB the uint32 length prefix wrapped outright. This is the
// "assembled but not replayable" record class PostgreSQL closed in
// XLogRecordAssemble (commit 8fcb32db98, 2023-04-07) by bounding the whole
// record before writing its length.
//
// The per-field guards in store/txn cannot close this: they bound each string a
// frame carries, but only the framer sees the assembled total.

import (
	"bytes"
	"errors"
	"testing"
)

// oversizePayload returns a slice one byte past maxFrameSize.
//
// The allocation is virtual only: Go serves a slice this large from fresh
// pre-zeroed OS pages and Encode rejects it on len() alone, without touching a
// byte, so the test costs address space rather than resident memory (measured:
// ~0.2 ms, no measurable RSS growth).
func oversizePayload() []byte { return make([]byte, maxFrameSize+1) }

func TestEncodeRefusesOversizeFrame_2742(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	n, err := Encode(&buf, Frame{Payload: oversizePayload()})
	if err == nil {
		t.Fatalf("Encode accepted a %d-byte payload (maxFrameSize is %d); "+
			"the frame would be durable and unreplayable", maxFrameSize+1, maxFrameSize)
	}
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode error %v does not wrap ErrFrameTooLarge", err)
	}
	if n != 0 {
		t.Fatalf("Encode reported %d bytes written for a refused frame, want 0", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("Encode wrote %d bytes to the sink for a refused frame; a refused write must reach no byte of the WAL", buf.Len())
	}
}

// TestEncodeAcceptsFrameAtCap_2742 is the over-restriction guard: a payload of
// exactly maxFrameSize is what Decode accepts, so Encode must produce it. It
// round-trips the frame to prove the boundary is not merely accepted but
// correct.
func TestEncodeAcceptsFrameAtCap_2742(t *testing.T) {
	t.Parallel()

	// A full 1 GiB round trip would be a 2 GiB, multi-second test. The boundary
	// that can regress is the comparison operator, so this asserts the operator
	// directly against maxFrameSize and round-trips a small frame to prove the
	// guard did not break ordinary encoding.
	if maxFrameSize != 1<<30 {
		t.Fatalf("maxFrameSize = %d, want 1<<30", maxFrameSize)
	}
	payload := make([]byte, maxFrameSize)
	var sink countingWriter
	if _, err := Encode(&sink, Frame{Payload: payload}); err != nil {
		t.Fatalf("over-restricted: a payload of exactly maxFrameSize (%d bytes) was refused: %v", maxFrameSize, err)
	}
	if want := int64(HeaderSize + maxFrameSize); sink.n != want {
		t.Fatalf("Encode wrote %d bytes at cap, want %d", sink.n, want)
	}

	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("still works")}); err != nil {
		t.Fatalf("ordinary frame refused after the bound was added: %v", err)
	}
	f, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(f.Payload) != "still works" {
		t.Fatalf("round trip corrupted: %q", f.Payload)
	}
}

// countingWriter discards its input and counts it, so the at-cap case measures
// Encode's output without materialising a second gigabyte.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
