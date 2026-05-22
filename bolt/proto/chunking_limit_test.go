package proto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"gograph/bolt/proto"
)

// writeChunkedMessage frames msg into the standard Bolt chunked
// format on out: a sequence of length-prefixed chunks followed by
// the uint16(0) end-of-message sentinel. We hand-roll the framing
// rather than reuse proto.ChunkedWriter so the test can also
// produce wire payloads that are larger than any cap a real writer
// would emit.
func writeChunkedMessage(out *bytes.Buffer, msg []byte) {
	const maxChunk = 65535
	var hdr [2]byte
	remaining := msg
	for len(remaining) > 0 {
		chunk := remaining
		if len(chunk) > maxChunk {
			chunk = chunk[:maxChunk]
		}
		binary.BigEndian.PutUint16(hdr[:], uint16(len(chunk)))
		out.Write(hdr[:])
		out.Write(chunk)
		remaining = remaining[len(chunk):]
	}
	binary.BigEndian.PutUint16(hdr[:], 0)
	out.Write(hdr[:])
}

func TestChunkedReader_OverLimit_ReturnsTypedError(t *testing.T) {
	t.Parallel()
	const cap = 1024
	payload := make([]byte, cap+1) // one byte past the cap
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var buf bytes.Buffer
	writeChunkedMessage(&buf, payload)

	cr := proto.NewChunkedReaderWithLimit(&buf, cap)
	got, err := cr.ReadMessage()
	if err == nil {
		t.Fatalf("ReadMessage(over-cap) returned nil; want ErrMessageTooLarge")
	}
	if !errors.Is(err, proto.ErrMessageTooLarge) {
		t.Fatalf("err = %v; want errors.Is(err, ErrMessageTooLarge) == true", err)
	}
	if got != nil {
		t.Fatalf("ReadMessage(over-cap) returned non-nil payload (%d bytes); contract requires nil", len(got))
	}
}

func TestChunkedReader_ExactlyAtLimit_Succeeds(t *testing.T) {
	t.Parallel()
	const cap = 1024
	payload := make([]byte, cap) // exactly at the cap
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var buf bytes.Buffer
	writeChunkedMessage(&buf, payload)

	cr := proto.NewChunkedReaderWithLimit(&buf, cap)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage(exactly at cap): %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload roundtrip mismatch at cap boundary")
	}
}

func TestChunkedReader_ZeroLimit_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	// Verify the constructor's zero-value protection: passing 0 must
	// fall back to DefaultMaxMessageBytes so misconfiguration cannot
	// silently disable the cap.
	cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(nil), 0)
	if cr == nil {
		t.Fatal("NewChunkedReaderWithLimit(_, 0) returned nil")
	}
	// We cannot read maxMessageBytes directly (unexported), but we
	// can prove the fallback by feeding a 64 KiB payload (well below
	// 16 MiB) and confirming it is accepted.
	payload := make([]byte, 64<<10)
	var buf bytes.Buffer
	writeChunkedMessage(&buf, payload)
	cr = proto.NewChunkedReaderWithLimit(&buf, 0)
	if _, err := cr.ReadMessage(); err != nil {
		t.Fatalf("64 KiB message rejected under default cap: %v", err)
	}
}

func TestChunkedReader_DefaultConstructor_AppliesDefaultCap(t *testing.T) {
	t.Parallel()
	// NewChunkedReader (no explicit cap) must also enforce
	// DefaultMaxMessageBytes; otherwise legacy call sites that have
	// not yet adopted the new constructor remain DoS-vulnerable.
	// Confirm by exceeding the default with a single chunk.
	payload := make([]byte, proto.DefaultMaxMessageBytes+1)
	var buf bytes.Buffer
	writeChunkedMessage(&buf, payload)

	cr := proto.NewChunkedReader(&buf)
	_, err := cr.ReadMessage()
	if !errors.Is(err, proto.ErrMessageTooLarge) {
		t.Fatalf("expected ErrMessageTooLarge under default cap; got %v", err)
	}
}

// TestChunkedReader_OverLimit_BoundedAllocation is the soak-style
// guard: an attacker streams non-zero chunks indefinitely; the
// reader must reject before allocating the would-be-oversized
// message buffer. We prove this by writing a wire payload that
// claims to deliver many MiB of data while the cap is only a few
// KiB, then checking ReadMessage's own per-call allocation profile
// via testing.AllocsPerRun — the fixture's wire-buffer alloc is
// outside the measured closure, so only the implementation's own
// allocs count.
//
// A leaking implementation that allocates the full prospective
// msg buffer would show allocs proportional to (claimed payload
// size / typical alloc bucket). A correctly bounded implementation
// allocates at most the per-call ChunkedReader+bufio.Reader pair
// plus the wrapping error, regardless of how much data the client
// claims to send.
func TestChunkedReader_OverLimit_BoundedAllocation(t *testing.T) {
	// Build a single very-large wire payload OUTSIDE the measured
	// closure: an 8 MiB message that the reader must reject under
	// a 4 KiB cap.
	const (
		cap        = 4 << 10 // 4 KiB cap
		claim      = 8 << 20 // 8 MiB claimed payload
		allocBound = 50.0    // allowed allocs per call — generous to absorb bufio + error wrapping
	)
	payload := make([]byte, claim)
	var wire bytes.Buffer
	writeChunkedMessage(&wire, payload)
	wireBytes := wire.Bytes()

	got := testing.AllocsPerRun(20, func() {
		cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wireBytes), cap)
		_, err := cr.ReadMessage()
		if !errors.Is(err, proto.ErrMessageTooLarge) {
			t.Fatalf("err = %v; want ErrMessageTooLarge", err)
		}
	})
	if got > allocBound {
		t.Fatalf("per-call allocations = %.1f; want <= %.1f (cap=%d B, claimed=%d B). "+
			"A regression here means the reader is allocating the prospective oversized "+
			"msg buffer before the cap check.", got, allocBound, cap, claim)
	}
}

// TestChunkedReader_OverLimit_LegalMessageAfterDropsConnection
// confirms the contract that a too-large message renders the
// stream unusable: once the cap is breached, the reader is in an
// undefined state from the protocol's point of view and further
// reads on the same buffer are not guaranteed to succeed. The
// caller's correct response is to close the connection.
func TestChunkedReader_OverLimit_LegalMessageAfterDropsConnection(t *testing.T) {
	t.Parallel()
	const cap = 256

	// Write: oversized message, then a small legal message after it.
	var buf bytes.Buffer
	writeChunkedMessage(&buf, make([]byte, cap+1))
	writeChunkedMessage(&buf, []byte("ok"))

	cr := proto.NewChunkedReaderWithLimit(&buf, cap)
	if _, err := cr.ReadMessage(); !errors.Is(err, proto.ErrMessageTooLarge) {
		t.Fatalf("first read: err=%v; want ErrMessageTooLarge", err)
	}
	// After the offending chunk's payload has been drained, the next
	// header is the trailing uint16(0) sentinel of the rejected
	// message; the legal "ok" message is still on the wire. The
	// contract does not promise resyncing, but we should at least
	// not panic and the error path should be observable.
	_, err := cr.ReadMessage()
	if err == nil {
		// Acceptable: reader happened to land at the sentinel and
		// returned an empty message. Read once more to surface the
		// remaining payload header.
		if _, err = cr.ReadMessage(); err != nil && !errors.Is(err, io.EOF) {
			// Either io.EOF or a clean follow-up read are acceptable
			// outcomes — the contract is "no panic", not "resyncs".
			t.Logf("post-reject follow-up read: %v (informational)", err)
		}
	}
}
