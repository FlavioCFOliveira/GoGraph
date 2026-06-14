package wal

// Security test battery — WAL frame-length bound through the Reader iterator.
//
// DEFENSE LOCK-IN (passes today): wal.Decode rejects a frame whose declared
// payload length exceeds maxFrameSize (1<<30) BEFORE the make([]byte, plen),
// returning ErrFrameTooLarge. format_maxframe_test.go pins that at the Decode
// level. This file pins the composed behaviour one layer up: when a hostile
// length sits on a MIDDLE frame of a multi-frame log, the Reader iterator
// yields every valid frame BEFORE it, then stops cleanly with
// TailError() == ErrFrameTooLarge — and the hostile frame's payload is never
// allocated. This is the property a recovery replay actually depends on: a
// crafted length deep in the WAL must truncate the replay at that point, not
// drive a multi-gigabyte allocation.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// secStoreWALFrameBytes encodes one valid frame and returns its bytes.
func secStoreWALFrameBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: payload}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf.Bytes()
}

// secStoreWALHostileFrameHeader builds a bare frame header (no payload bytes
// follow) declaring a payload length one past the maxFrameSize ceiling. The
// CRC field is left zero: Decode checks the length cap before the CRC, so the
// frame is rejected without the checksum mattering.
func secStoreWALHostileFrameHeader() []byte {
	head := make([]byte, HeaderSize)
	copy(head[0:4], Magic[:])
	binary.LittleEndian.PutUint16(head[4:6], CurrentVersion)
	binary.LittleEndian.PutUint32(head[6:10], maxFrameSize+1)
	return head
}

// TestSec_Store_WALReaderStopsAtHostileFrameLength assembles a log of two
// valid frames followed by a frame header declaring an over-ceiling payload
// length (and no payload bytes). The Reader must yield exactly the two valid
// frames, then stop with TailError() == ErrFrameTooLarge, having allocated
// nothing for the hostile frame.
func TestSec_Store_WALReaderStopsAtHostileFrameLength(t *testing.T) {
	t.Parallel()

	valid := [][]byte{[]byte("valid-frame-0"), []byte("valid-frame-1")}
	var log bytes.Buffer
	for _, p := range valid {
		log.Write(secStoreWALFrameBytes(t, p))
	}
	hostileOffset := log.Len()
	log.Write(secStoreWALHostileFrameHeader())

	r := NewReader(bytes.NewReader(log.Bytes()), nil)
	seen := 0
	for f := range r.Frames() {
		if seen >= len(valid) {
			t.Fatalf("Reader yielded an extra frame %d past the valid prefix; payload=%q", seen, f.Payload)
		}
		if !bytes.Equal(f.Payload, valid[seen]) {
			t.Fatalf("frame %d payload = %q, want %q", seen, f.Payload, valid[seen])
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("Reader yielded %d frames, want 2 (the valid prefix before the hostile frame)", seen)
	}
	if !errors.Is(r.TailError(), ErrFrameTooLarge) {
		t.Fatalf("TailError = %v, want ErrFrameTooLarge", r.TailError())
	}
	// The iterator stopped at the start of the hostile frame, so TailOffset
	// points there — proving it did not consume (or allocate) the declared
	// payload.
	if r.TailOffset() != int64(hostileOffset) {
		t.Fatalf("TailOffset = %d, want %d (start of the hostile frame)", r.TailOffset(), hostileOffset)
	}
}

// TestSec_Store_WALReplayRejectsHostileLengthWithoutAllocation drives Replay
// over a single hostile frame header whose declared length is the maximum a
// uint32 can hold (~4 GiB), supplying only the header bytes (no payload). The
// cap must reject the frame before the make([]byte, plen): Replay surfaces
// ErrFrameTooLarge and applies no frames. Because only HeaderSize bytes back
// the reader, reaching the payload read would instead surface ErrTornFrame —
// so an ErrFrameTooLarge result is positive proof the cap short-circuited
// before any payload allocation. Asserting via Replay (not bare Decode)
// confirms the recovery-facing API inherits the cap.
func TestSec_Store_WALReplayRejectsHostileLengthWithoutAllocation(t *testing.T) {
	t.Parallel()

	head := make([]byte, HeaderSize)
	copy(head[0:4], Magic[:])
	binary.LittleEndian.PutUint16(head[4:6], CurrentVersion)
	binary.LittleEndian.PutUint32(head[6:10], 0xFFFFFFFF) // ~4 GiB declared payload

	r := NewReader(bytes.NewReader(head), nil)
	applied := 0
	err := r.Replay(func(Frame) error {
		applied++
		return nil
	})
	if err == nil {
		t.Fatal("Replay accepted a frame declaring a ~4 GiB payload; want ErrFrameTooLarge")
	}
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Replay error = %v, want ErrFrameTooLarge (a torn-tail result would mean the payload read was reached)", err)
	}
	if applied != 0 {
		t.Fatalf("Replay applied %d frames; want 0 (the hostile frame must not be decoded)", applied)
	}
}
