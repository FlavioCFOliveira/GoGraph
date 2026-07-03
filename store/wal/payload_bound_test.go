package wal

// payload_bound_test.go — regression gate for the WAL-payload sibling of the
// snapshot value-alloc bound: Decode must not eagerly reserve an untrusted plen
// (up to maxFrameSize = 1 GiB). A forged/tampered frame header that
// over-declares plen past EOF must fail as a torn frame without a speculative
// large allocation.

import (
	"bytes"
	"errors"
	"testing"
)

// TestDecode_ForgedLargePlen_TornNotHuge feeds a frame header declaring a plen
// far above framePayloadEagerCap but backed by only a few benign payload bytes.
// The bytes embed no valid frame, so Decode must classify it as a torn tail
// (ErrTornFrame) — read bounded, not the declared plen.
func TestDecode_ForgedLargePlen_TornNotHuge(t *testing.T) {
	t.Parallel()
	const plen = 256 << 20 // > framePayloadEagerCap, < maxFrameSize
	payload := []byte("only-a-few-bytes-not-a-frame")
	buf := make([]byte, HeaderSize+len(payload))
	putCandidateHeader(buf, 0, CurrentVersion, plen, 0xDEADBEEF)
	copy(buf[HeaderSize:], payload)

	if _, err := Decode(bytes.NewReader(buf)); !errors.Is(err, ErrTornFrame) {
		t.Fatalf("Decode(forged large plen, short benign payload) = %v, want ErrTornFrame", err)
	}
}

// TestDecode_ForgedLargePlen_MasksDataStillDetected confirms the bounded read
// preserves the anti-silent-data-loss signal: if the over-long read consumes
// bytes that embed a genuine CRC-valid frame, Decode still promotes it to
// ErrTornFrameMasksData (fail-stop) rather than accepting a truncated prefix.
func TestDecode_ForgedLargePlen_MasksDataStillDetected(t *testing.T) {
	t.Parallel()
	// A real, CRC-valid inner frame with a tiny payload.
	inner := []byte("committed")
	innerFrame := make([]byte, HeaderSize+len(inner))
	putCandidateHeader(innerFrame, 0, CurrentVersion, uint32(len(inner)), crc32Header(CurrentVersion, uint32(len(inner)), inner))
	copy(innerFrame[HeaderSize:], inner)

	// Outer frame over-declares plen but is backed only by the inner frame bytes.
	const plen = 8 << 20 // > framePayloadEagerCap so it takes the grow path
	buf := make([]byte, HeaderSize+len(innerFrame))
	putCandidateHeader(buf, 0, CurrentVersion, plen, 0xFEEDFACE)
	copy(buf[HeaderSize:], innerFrame)

	if _, err := Decode(bytes.NewReader(buf)); !errors.Is(err, ErrTornFrameMasksData) {
		t.Fatalf("Decode(over-long plen swallowing a valid frame) = %v, want ErrTornFrameMasksData", err)
	}
}
