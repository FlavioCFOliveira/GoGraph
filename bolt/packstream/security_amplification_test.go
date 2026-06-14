package packstream_test

// security_amplification_test.go — DEFENSE LOCK-IN matrix for the PackStream
// decoder's pre-allocation budgets (security audit, Bolt/protocol cluster).
//
// The decoder defends two distinct amplification classes, each with its own
// typed error:
//
//   - ErrLengthExceedsInput — a length/count prefix claims more on-wire bytes
//     than the message can possibly contain (a 5-byte frame requesting a
//     multi-GiB Bytes/String payload or billions of List/Map slots), OR a
//     32-bit prefix above MaxInt32 (which would wrap negative on a 32-bit int);
//   - ErrDecodedMemoryExceeded — a structurally VALID, fully-accounted message
//     whose decoded collection storage would still blow the 128 MiB per-message
//     decoded-memory budget (e.g. a 16 MiB List32 of NULLs amplifying ~16x).
//
// Individual tests already exist per arm (length_bound_test.go,
// length_cast_test.go, decoded_budget_test.go). This file adds the consolidated
// adversarial matrix the audit calls for: every wire vector (Bytes32 / Str32 /
// List32 / Map32) at and above each boundary, each mapped to the exact error it
// must produce. It is the single place a reviewer reads to confirm no vector
// is left without a budget, and a regression that drops one arm's guard fails a
// named subtest here.
//
// These tests are deliberately NOT parallel and assert only the typed error —
// never a runtime.MemStats allocation delta — because TotalAlloc is a
// process-global counter that flakes under parallel/-race execution.

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// secAmpHeader32 builds a 5-byte collection/scalar header: one marker byte
// followed by a big-endian uint32 length/count prefix. No payload follows, so a
// prefix that survives the wire byte budget would request an allocation the
// frame cannot back.
func secAmpHeader32(marker byte, n uint32) []byte {
	return appendUint32([]byte{marker}, n)
}

// TestSec_Bolt_PackstreamAmplificationMatrix is the consolidated gate: each
// oversize 32-bit prefix is rejected before allocation with the documented
// typed error. The cases at 0xFFFFFFFF and 0x80000000 sit in the int-wrap range
// — they must be caught by the unsigned MaxInt32 cap (ErrLengthExceedsInput) on
// every platform — while the "just-claims-too-much" cases (a modest count that
// still exceeds a 5-byte frame's bytes-remaining) exercise the bytes-remaining
// guard.
func TestSec_Bolt_PackstreamAmplificationMatrix(t *testing.T) {
	read := map[string]func(*packstream.Decoder) error{
		"Bytes": func(d *packstream.Decoder) error { _, err := d.ReadBytes(); return err },
		"Str":   func(d *packstream.Decoder) error { _, err := d.ReadString(); return err },
		"List":  func(d *packstream.Decoder) error { _, err := d.ReadListHeader(); return err },
		"Map":   func(d *packstream.Decoder) error { _, err := d.ReadMapHeader(); return err },
	}
	markers := map[string]byte{
		"Bytes": markerBytes32,
		"Str":   markerStr32,
		"List":  markerList32,
		"Map":   markerMap32,
	}

	cases := []struct {
		name string
		n    uint32
	}{
		// Int-wrap range: smallest value that wraps a 32-bit int, and the max.
		{"wrap_min_0x80000000", 0x80000000},
		{"wrap_max_0xFFFFFFFF", 0xFFFFFFFF},
		// Below the wrap but far beyond a 5-byte frame's remaining bytes:
		// the bytes-remaining guard (ErrLengthExceedsInput) must reject these
		// without ever charging the decoded budget.
		{"claims_1MiB_no_payload", 1 << 20},
		{"claims_64KiB_no_payload", 1 << 16},
	}

	for _, arm := range []string{"Bytes", "Str", "List", "Map"} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s_%s", arm, tc.name), func(t *testing.T) {
				frame := secAmpHeader32(markers[arm], tc.n)
				dec := packstream.NewDecoder(bytes.NewReader(frame))
				err := read[arm](dec)
				if !errors.Is(err, packstream.ErrLengthExceedsInput) {
					t.Fatalf("%s prefix 0x%08X: error = %v, want ErrLengthExceedsInput before allocation",
						arm, tc.n, err)
				}
			})
		}
	}
}

// TestSec_Bolt_PackstreamDecodedBudgetMatrix is the structurally-valid
// amplification gate: a message whose every byte is accounted for on the wire
// can still demand decoded storage far beyond its wire size. Both the List32 of
// NULLs (~16x) and the Map32 of empty-string/NULL pairs (~24x) must be rejected
// at the collection header with ErrDecodedMemoryExceeded. The frames here lift
// the byte budget out of the way (SetUnboundedBudgetForTest) so the
// decoded-memory budget — not the wire byte budget — is unambiguously the guard
// that fires. The declared counts are large enough to exceed the 128 MiB
// decoded budget but small on the wire, so no large allocation is attempted.
func TestSec_Bolt_PackstreamDecodedBudgetMatrix(t *testing.T) {
	budget := packstream.MaxDecodedCollectionBytesForTest()
	listElem := packstream.ListElemCostForTest()
	mapEntry := packstream.MapEntryCostForTest()

	// One element/entry beyond what the budget can hold.
	overList := uint32(budget/listElem + 1)
	overMap := uint32(budget/mapEntry + 1)

	cases := []struct {
		name   string
		marker byte
		n      uint32
		read   func(*packstream.Decoder) error
	}{
		{"List32_NULLs_over_budget", markerList32, overList, func(d *packstream.Decoder) error { _, err := d.ReadListHeader(); return err }},
		{"Map32_empty_pairs_over_budget", markerMap32, overMap, func(d *packstream.Decoder) error { _, err := d.ReadMapHeader(); return err }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := packstream.NewDecoder(bytes.NewReader(secAmpHeader32(tc.marker, tc.n)))
			// Take the wire byte budget out of the equation so the decoded-memory
			// budget is the only guard that can fire.
			dec.SetUnboundedBudgetForTest()
			err := tc.read(dec)
			if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
				t.Fatalf("%s (count %d): error = %v, want ErrDecodedMemoryExceeded at the header",
					tc.name, tc.n, err)
			}
		})
	}
}

// TestSec_Bolt_PackstreamHeaderAtBudgetBoundaryAccepted pins the safe side of
// the decoded-memory boundary: a List header whose charge exactly fits the
// remaining budget must be accepted (the guard rejects strictly-over, not
// at-boundary), so the budget does not over-reject legitimate dense traffic.
// The header is read in isolation via the exported ReadListHeader, so no
// element payload is required.
func TestSec_Bolt_PackstreamHeaderAtBudgetBoundaryAccepted(t *testing.T) {
	budget := packstream.MaxDecodedCollectionBytesForTest()
	collCost := packstream.CollectionCostForTest()
	listElem := packstream.ListElemCostForTest()

	// Largest element count that still fits: (budget - collectionCost) / perElem.
	fit := uint32((budget - collCost) / listElem)

	dec := packstream.NewDecoder(bytes.NewReader(secAmpHeader32(markerList32, fit)))
	dec.SetUnboundedBudgetForTest()
	n, err := dec.ReadListHeader()
	if err != nil {
		t.Fatalf("List header at the budget boundary (count %d) was rejected: %v", fit, err)
	}
	if n != int(fit) {
		t.Fatalf("List header count: got %d, want %d", n, fit)
	}
}

// TestSec_Bolt_PackstreamMaxInt32BoundaryNotOverRejected pins the int-wrap cap's
// safe edge: a prefix of exactly MaxInt32 (0x7FFFFFFF) — the largest value that
// cannot wrap a signed int — must NOT be rejected by the pre-conversion cap.
// With the wire byte budget lifted it falls through to the decoded-memory
// budget (for List/Map) or the bytes-remaining make-guard (for Bytes/Str). The
// point asserted here is only that the error is NOT the wrong one: the cap must
// compose with, not pre-empt, the downstream budgets.
func TestSec_Bolt_PackstreamMaxInt32BoundaryNotOverRejected(t *testing.T) {
	const maxInt32 = uint32(0x7FFFFFFF)

	// List at MaxInt32: byte budget lifted → must reach the decoded-memory budget.
	t.Run("List_MaxInt32_reaches_decoded_budget", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(secAmpHeader32(markerList32, maxInt32)))
		dec.SetUnboundedBudgetForTest()
		_, err := dec.ReadListHeader()
		if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
			t.Fatalf("List count 0x7FFFFFFF: error = %v, want ErrDecodedMemoryExceeded (cap must not pre-empt the decoded budget)", err)
		}
		if errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatal("List count 0x7FFFFFFF must not be rejected by the MaxInt32 cap (it does not wrap)")
		}
	})
}
