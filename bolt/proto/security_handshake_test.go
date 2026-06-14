package proto_test

// security_handshake_test.go — DEFENSE LOCK-IN for the Bolt version handshake
// (security audit, Bolt/protocol cluster).
//
// The handshake is the first thing an unauthenticated peer touches, so it must
// never panic, never block on a short payload, and never mis-parse a malformed
// version slot into an out-of-range computation. The existing proto_test.go and
// handshake_vectors_test.go cover the happy negotiation vectors, bad magic, and
// context cancellation. This file pins the adversarial edges those do not:
//
//   - a truncated (< 20-byte) handshake yields io.ErrUnexpectedEOF, not a hang
//     or panic;
//   - a slot whose minor_range exceeds its minor (which would underflow an
//     unsigned subtraction) is parsed without panicking and does not falsely
//     match a higher version than offered;
//   - a structurally malformed payload returns a typed error, never a panic;
//   - an all-zero / unknown-only offer is rejected with [0,0,0,0] on the wire
//     and ErrNoCommonVersion.
//
// Layer: short. Each subtest drives proto.Negotiate over a net.Pipe with a
// goroutine that is always joined, so no goroutine leaks.

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// secProtoNegotiateRaw drives the server side of Negotiate against a client that
// writes exactly raw (which may be a truncated or malformed handshake) and then
// reads up to 4 response bytes. It returns the negotiated version, the raw
// response bytes the client observed (the zero value when the server wrote
// nothing), and the negotiation error. The client goroutine is always joined
// before return.
func secProtoNegotiateRaw(t *testing.T, raw []byte) (proto.Version, [4]byte, error) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	respCh := make(chan [4]byte, 1)
	go func() {
		if _, err := client.Write(raw); err != nil {
			close(respCh)
			return
		}
		// The server may close without writing (truncated/rejected); a short read
		// is fine. Bound the read so a non-writing server cannot wedge the client.
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		var r [4]byte
		_, _ = io.ReadFull(client, r[:])
		respCh <- r
	}()

	// Bound the server side so a malformed/blocking payload cannot wedge the test.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := proto.Negotiate(ctx, server)

	resp := <-respCh
	return v, resp, err
}

// TestSec_Bolt_HandshakeTruncatedNoHang verifies that a handshake shorter than
// 20 bytes is reclaimed as io.ErrUnexpectedEOF rather than blocking forever or
// panicking. A pre-auth peer that sends a partial preamble and then closes must
// not leave Negotiate stuck in io.ReadFull.
func TestSec_Bolt_HandshakeTruncatedNoHang(t *testing.T) {
	t.Parallel()

	// Valid magic but only 8 of the 20 bytes (magic + one partial slot), then EOF.
	raw := []byte{0x60, 0x60, 0xB0, 0x17, 0x00, 0x00, 0x00}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() {
		_, _ = client.Write(raw)
		_ = client.Close() // signal EOF after the short write
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := proto.Negotiate(ctx, server)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated handshake: error = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestSec_Bolt_HandshakeMinorRangeUnderflowGuarded pins the underflow guard in
// the slot parser: a slot offering minor=0 with minor_range=255 must not
// underflow the unsigned (minor - minor_range) subtraction into a huge minMinor
// that would either reject a legitimate exact match or match a version never
// offered. The slot [0x00, range=255, minor=0, major=5] means "v5.0 only"
// because the range is clamped, so the server must agree on exactly v5.0.
func TestSec_Bolt_HandshakeMinorRangeUnderflowGuarded(t *testing.T) {
	t.Parallel()

	// [pad, minor_range=255, minor=0, major=5]: range > minor would underflow.
	raw := buildHandshake(proto.Magic, [][4]byte{{0, 255, 0, 5}})

	v, resp, err := secProtoNegotiateRaw(t, raw)
	if err != nil {
		t.Fatalf("underflow-range slot: unexpected error %v", err)
	}
	if v.Major != 5 || v.Minor != 0 {
		t.Fatalf("underflow-range slot: agreed v%d.%d, want v5.0 (range clamped, not underflowed)", v.Major, v.Minor)
	}
	if resp != ([4]byte{0, 0, 0, 5}) {
		t.Fatalf("underflow-range slot: wire response %v, want [0 0 0 5]", resp)
	}
}

// TestSec_Bolt_HandshakeMinorRangeUnderflowHighMinor is the companion: a slot
// with minor=2, minor_range=255 must be treated as "v5.0 .. v5.2" (the low end
// clamped to 0), never as a window starting at a wrapped, near-max minor. The
// server picks its highest supported minor within [0, 2] for major 5, i.e.
// v5.2.
func TestSec_Bolt_HandshakeMinorRangeUnderflowHighMinor(t *testing.T) {
	t.Parallel()

	// [pad, minor_range=255, minor=2, major=5]: clamps to v5.0..v5.2.
	raw := buildHandshake(proto.Magic, [][4]byte{{0, 255, 2, 5}})

	v, _, err := secProtoNegotiateRaw(t, raw)
	if err != nil {
		t.Fatalf("high-minor underflow slot: unexpected error %v", err)
	}
	if v.Major != 5 || v.Minor != 2 {
		t.Fatalf("high-minor underflow slot: agreed v%d.%d, want v5.2", v.Major, v.Minor)
	}
}

// TestSec_Bolt_HandshakeUnknownVersionsRejected pins that an offer containing
// only versions the server does not support is rejected with the [0,0,0,0] wire
// sentinel and ErrNoCommonVersion — no panic, no accidental match. It folds the
// all-zero (empty-offer) and unknown-major cases into one table.
func TestSec_Bolt_HandshakeUnknownVersionsRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		slots [][4]byte
	}{
		{"all_zero_slots", [][4]byte{{0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}}},
		{"unknown_major_3", [][4]byte{{0, 0, 0, 3}}},
		{"unknown_major_9_with_range", [][4]byte{{0, 9, 9, 9}}},
		{"future_bolt_6", [][4]byte{{0, 0, 0, 6}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := buildHandshake(proto.Magic, tc.slots)
			v, resp, err := secProtoNegotiateRaw(t, raw)
			if !errors.Is(err, proto.ErrNoCommonVersion) {
				t.Fatalf("%s: error = %v, want ErrNoCommonVersion (agreed v%d.%d)", tc.name, err, v.Major, v.Minor)
			}
			if resp != ([4]byte{0, 0, 0, 0}) {
				t.Fatalf("%s: wire response %v, want the [0 0 0 0] rejection sentinel", tc.name, resp)
			}
		})
	}
}

// TestSec_Bolt_HandshakeMalformedNoPanic is a robustness sweep: a spread of
// odd-but-magic-valid 20-byte payloads must each return a typed error or a
// version without panicking. The test asserts only that Negotiate returns
// (never panics or hangs); the precise outcome per payload is exercised by the
// dedicated tests above. Recovering a panic here turns a crash into a test
// failure with the offending payload attached.
func TestSec_Bolt_HandshakeMalformedNoPanic(t *testing.T) {
	t.Parallel()

	payloads := [][][4]byte{
		{{0xFF, 0xFF, 0xFF, 0xFF}},                  // every byte set in slot 0
		{{0x01, 0x02, 0x03, 0x05}},                  // non-zero padding byte
		{{0, 0, 0, 5}, {0xFF, 0xFF, 0xFF, 0xFF}},    // valid then garbage
		{{0, 0, 0xFF, 5}},                           // minor=255, range=0
		{{0xAB, 0xCD, 0xEF, 0x05}, {0, 0, 0, 0x04}}, // junk slot then v4.x major
	}

	for i, slots := range payloads {
		i, slots := i, slots
		t.Run(secProtoName(i), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Negotiate panicked on malformed payload %v: %v", slots, r)
				}
			}()
			raw := buildHandshake(proto.Magic, slots)
			// We do not assert the outcome — only that Negotiate returns without
			// panicking or hanging. secProtoNegotiateRaw bounds both sides.
			_, _, _ = secProtoNegotiateRaw(t, raw)
		})
	}
}

// secProtoName returns a stable subtest name for the malformed-payload sweep.
func secProtoName(i int) string {
	return "malformed_payload_" + string(rune('A'+i))
}
