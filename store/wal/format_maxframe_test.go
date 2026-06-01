package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// makeHeader builds a bare frame header (no payload) with the given
// version and declared payload length. The CRC field is left zero: the
// length cap is checked before the CRC, so these headers exercise the
// guard without needing a valid checksum.
func makeHeader(version uint16, plen uint32) []byte {
	head := make([]byte, HeaderSize)
	copy(head[0:4], Magic[:])
	binary.LittleEndian.PutUint16(head[4:6], version)
	binary.LittleEndian.PutUint32(head[6:10], plen)
	return head
}

// oneByteReader yields the header bytes exactly, then reports a clear
// failure if Decode ever tries to read a payload. It exists to prove
// Decode rejects an over-large frame before attempting the large
// io.ReadFull (and thus before the make), since reaching a payload read
// would mean the cap did not short-circuit.
type oneByteReader struct {
	data []byte
	t    *testing.T
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		r.t.Fatalf("Decode read past the header: the frame-size cap did not short-circuit before the payload read")
		return 0, errReader{err: errors.New("unreachable")}.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestDecode_RejectsOversizedFrameBeforeAllocation(t *testing.T) {
	t.Parallel()
	// A header claiming a payload one byte past the ceiling. The reader
	// supplies only the header; if Decode honoured the (huge) length it
	// would attempt to read a payload and trip oneByteReader's guard.
	head := makeHeader(CurrentVersion, maxFrameSize+1)
	r := &oneByteReader{data: head, t: t}
	_, err := Decode(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Decode oversized frame = %v, want ErrFrameTooLarge", err)
	}
}

func TestDecode_RejectsMaxUint32LengthBeforeAllocation(t *testing.T) {
	t.Parallel()
	// The pathological worst case: the length field is the maximum a
	// uint32 can hold (~4 GiB). Decode must reject it via the cap, never
	// allocating, never reading the payload.
	head := makeHeader(CurrentVersion, 0xFFFFFFFF)
	r := &oneByteReader{data: head, t: t}
	_, err := Decode(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Decode max-uint32 frame = %v, want ErrFrameTooLarge", err)
	}
}

func TestDecode_AcceptsFrameAtCeilingBoundary(t *testing.T) {
	t.Parallel()
	// A header declaring exactly maxFrameSize bytes is within policy and
	// must pass the cap. We do not supply the (1 GiB) payload, so Decode
	// proceeds to read it and stops at ErrTornFrame — proving the cap let
	// the frame through rather than rejecting it as ErrFrameTooLarge.
	head := makeHeader(CurrentVersion, maxFrameSize)
	_, err := Decode(bytes.NewReader(head))
	if errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Decode at-ceiling frame rejected by cap; want it to pass the cap")
	}
	if !errors.Is(err, ErrTornFrame) {
		t.Fatalf("Decode at-ceiling header-only = %v, want ErrTornFrame", err)
	}
}

func TestDecode_NormalFrameStillDecodesAfterCap(t *testing.T) {
	t.Parallel()
	// Positive control: an ordinary frame round-trips unchanged with the
	// cap in place.
	in := Frame{Payload: []byte("a perfectly ordinary frame")}
	var buf bytes.Buffer
	if _, err := Encode(&buf, in); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("Payload mismatch: %q vs %q", out.Payload, in.Payload)
	}
}
