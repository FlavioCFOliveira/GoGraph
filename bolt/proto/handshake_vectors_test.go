package proto_test

// T651 — Bolt v5 handshake selects latest mutual version.
//
// The existing TestNegotiateV54 covers a single exact point. This file adds
// four representative offer vectors to fully satisfy the AC:
//   1. Range offer spanning several v5 minors: server must pick the highest.
//   2. Multiple slots: server must pick the highest mutual across all slots.
//   3. Single exact v5.0 offer: no range, no alternatives.
//   4. Exact v5.6 offer: highest server-supported minor today.
//
// Each subtest also verifies the exact wire format of the four-byte response
// (AC #2: server returns [major, minor, 0, 0] in big-endian order).

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"gograph/bolt/proto"
)

// runNegotiate drives the server side of a handshake and returns the agreed
// Version together with the raw 4-byte response read by the client side.
func runNegotiate(t *testing.T, offerSlots [][4]byte) (v proto.Version, rawResp [4]byte) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	respCh := make(chan [4]byte, 1)
	go func() {
		payload := buildHandshake(proto.Magic, offerSlots)
		if _, err := client.Write(payload); err != nil {
			close(respCh)
			return
		}
		var raw [4]byte
		if _, err := io.ReadFull(client, raw[:]); err != nil {
			close(respCh)
			return
		}
		respCh <- raw
	}()

	v, err := proto.Negotiate(context.Background(), server)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}

	rawResp, ok := <-respCh
	if !ok {
		t.Fatal("client goroutine failed to read the 4-byte response")
	}
	return v, rawResp
}

// TestNegotiateV5HighestMutualRangeOffer covers offer vector #1:
// client sends [5, 6, 6, 0] meaning "I support v5.0 through v5.6".
// The server must select its highest supported v5 version (currently v5.6).
func TestNegotiateV5HighestMutualRangeOffer(t *testing.T) {
	t.Parallel()

	// Slot: [major=5, minor=6, minor_range=6, 0] → accepts [5.0, 5.6].
	v, rawResp := runNegotiate(t, [][4]byte{{5, 6, 6, 0}})

	// Server must choose the highest mutual version — v5.6 in this case.
	if v.Major != 5 || v.Minor != 6 {
		t.Errorf("want v5.6, got v%d.%d", v.Major, v.Minor)
	}

	// AC #2: wire response must be [5, 6, 0, 0].
	want := [4]byte{5, 6, 0, 0}
	if rawResp != want {
		t.Errorf("wire response: want %v, got %v", want, rawResp)
	}

	// Validate that the four bytes are big-endian: bytes 2 and 3 must be zero.
	if binary.BigEndian.Uint16(rawResp[2:]) != 0 {
		t.Errorf("response bytes [2:4] must be zero, got 0x%04X", binary.BigEndian.Uint16(rawResp[2:]))
	}
}

// TestNegotiateV5MultipleSlotsBestPick covers offer vector #2:
// client sends two slots; the server must select the highest mutual version
// across both slots, not just the first match.
//
// Slots:
//   - [5, 0, 0, 0] — only v5.0
//   - [5, 6, 2, 0] — v5.4 through v5.6
//
// Server should pick v5.6 (higher than v5.0 from the first slot).
func TestNegotiateV5MultipleSlotsBestPick(t *testing.T) {
	t.Parallel()

	v, rawResp := runNegotiate(t, [][4]byte{
		{5, 0, 0, 0}, // slot 1: v5.0 only
		{5, 6, 2, 0}, // slot 2: v5.4–v5.6
	})

	if v.Major != 5 || v.Minor != 6 {
		t.Errorf("want v5.6 (best from slot 2), got v%d.%d", v.Major, v.Minor)
	}
	if rawResp != ([4]byte{5, 6, 0, 0}) {
		t.Errorf("wire response: want [5 6 0 0], got %v", rawResp)
	}
}

// TestNegotiateV5ExactSinglePoint covers offer vector #3:
// client offers exactly v5.0 with no range. Server must agree on v5.0.
func TestNegotiateV5ExactSinglePoint(t *testing.T) {
	t.Parallel()

	v, rawResp := runNegotiate(t, [][4]byte{{5, 0, 0, 0}})

	if v.Major != 5 || v.Minor != 0 {
		t.Errorf("want v5.0, got v%d.%d", v.Major, v.Minor)
	}
	if rawResp != ([4]byte{5, 0, 0, 0}) {
		t.Errorf("wire response: want [5 0 0 0], got %v", rawResp)
	}
}

// TestNegotiateV5HighestServerMinor covers offer vector #4:
// client offers exactly v5.6 (highest server-supported minor today).
// Server must agree on v5.6 and return the exact wire bytes.
func TestNegotiateV5HighestServerMinor(t *testing.T) {
	t.Parallel()

	v, rawResp := runNegotiate(t, [][4]byte{{5, 6, 0, 0}})

	if v.Major != 5 || v.Minor != 6 {
		t.Errorf("want v5.6, got v%d.%d", v.Major, v.Minor)
	}

	// AC #2: wire format is [major, minor, 0, 0].
	want := [4]byte{5, 6, 0, 0}
	if rawResp != want {
		t.Errorf("wire response: want %v, got %v", want, rawResp)
	}
}
