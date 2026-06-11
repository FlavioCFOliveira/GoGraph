package packstream_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// These tests are the regression gate for the decoded-memory amplification
// fix (rmp #1349, 2026-06-10 audit): the wire byte budget bounds what a
// count prefix may claim, but a structurally VALID message can still amplify
// ~16x in memory, because a list element can be one wire byte (0xC0 NULL)
// while its decoded slot costs at least 16 bytes. A 16 MiB message of ~16.7M
// NULLs forced make([]Value, ~16.7M) — roughly 256 MB — per message, per
// connection. The decoder now charges every collection header against a
// cumulative per-message decoded-memory budget and rejects the excess with
// ErrDecodedMemoryExceeded before allocating.

// Additional marker bytes for hand-crafted frames (markerList32/markerMap32
// are declared in length_bound_test.go, markerNull in nesting_depth_test.go —
// same package). Values are from the PackStream v2 / Bolt v5 specification.
const (
	tinyList15  = 0x9F // TinyList of 15 elements.
	tinyStrZero = 0x80 // TinyString of length 0: a one-byte map key.
)

// maxMessageBytes mirrors proto.DefaultMaxMessageBytes (16 MiB): the largest
// message a Bolt client may deliver, and therefore the scale at which the
// amplification attack operates.
const maxMessageBytes = 16 << 20

// appendUint32 appends v big-endian. Taking uint32 directly lets call sites
// pass untyped constants without a lossy conversion.
func appendUint32(b []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(b, v)
}

// TestDecodedBudgetMaxSizeNullList is the core gate: a maximum-size (16 MiB)
// message that is a single List32 of ~16.7M NULLs. Every byte is accounted
// for on the wire, so the byte budget accepts it — yet decoding it would
// commit make([]Value, 16777211), ~256 MB of slots, a ~16x amplification.
// The decoded-memory budget must reject it with ErrDecodedMemoryExceeded
// instead, without first making the oversized allocation.
func TestDecodedBudgetMaxSizeNullList(t *testing.T) {
	const n = maxMessageBytes - 5 // List32 header is 5 bytes; the rest is NULLs.
	frame := make([]byte, 0, maxMessageBytes)
	frame = append(frame, markerList32)
	frame = appendUint32(frame, n)
	frame = append(frame, bytes.Repeat([]byte{markerNull}, n)...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
			t.Fatalf("ReadValue error = %v, want ErrDecodedMemoryExceeded (value type %T)", err, v)
		}
	})
}

// TestDecodedBudgetMaxSizeTinyPairMap is the map flavour of the gate: a
// maximum-size message that is a single Map32 of ~8.4M two-byte entries
// (empty-string key + NULL value). The byte budget accepts it (each entry is
// exactly its two-byte minimum), yet make(map[string]Value, 8388605) would
// pre-size hundreds of MB of buckets — a ~24x amplification. The
// decoded-memory budget must reject it before allocating.
func TestDecodedBudgetMaxSizeTinyPairMap(t *testing.T) {
	const pairs = (maxMessageBytes - 5) / 2
	frame := make([]byte, 0, maxMessageBytes)
	frame = append(frame, markerMap32)
	frame = appendUint32(frame, pairs)
	frame = append(frame, bytes.Repeat([]byte{tinyStrZero, markerNull}, pairs)...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
			t.Fatalf("ReadValue error = %v, want ErrDecodedMemoryExceeded (value type %T)", err, v)
		}
	})
}

// TestDecodedBudgetCumulativeAcrossNesting proves the budget is one running
// account for the whole message, not a per-collection check: an outer
// TinyList of 15 sibling List32 collections, each declaring 1 Mi NULLs. Each
// sibling charges ~16 MiB of decoded slots — individually well within the
// budget — but cumulatively they claim ~240 MiB, so the decode must fail
// with ErrDecodedMemoryExceeded partway through rather than materialising
// ~15.7M slots from a ~15.7 MB message.
func TestDecodedBudgetCumulativeAcrossNesting(t *testing.T) {
	const innerN = 1 << 20
	inner := make([]byte, 0, innerN+5)
	inner = append(inner, markerList32)
	inner = appendUint32(inner, innerN)
	inner = append(inner, bytes.Repeat([]byte{markerNull}, innerN)...)

	frame := make([]byte, 0, 15*len(inner)+1)
	frame = append(frame, tinyList15)
	for range 15 {
		frame = append(frame, inner...)
	}
	if len(frame) > maxMessageBytes {
		t.Fatalf("test frame is %d bytes, exceeds the %d message cap it must fit", len(frame), maxMessageBytes)
	}

	dec := packstream.NewDecoder(bytes.NewReader(frame))
	v, err := dec.ReadValue()
	if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
		t.Fatalf("ReadValue error = %v, want ErrDecodedMemoryExceeded (value type %T)", err, v)
	}
}

// TestDecodedBudgetListAtBoundaryDecodes is the positive pin on the
// documented ceiling: a list whose charge lands exactly on the
// decoded-memory budget must still decode. This guards against off-by-one
// over-rejection and pins the budget arithmetic end to end.
func TestDecodedBudgetListAtBoundaryDecodes(t *testing.T) {
	maxN := (packstream.MaxDecodedCollectionBytesForTest() - packstream.CollectionCostForTest()) /
		packstream.ListElemCostForTest()
	// The message-cap guard also bounds the uint32 conversion below:
	// maxN <= maxMessageBytes-5 (16 MiB - 5), far below MaxUint32 on every
	// platform. (A `maxN > math.MaxUint32` clause would not even compile on
	// a 32-bit build, where the constant overflows int.)
	if maxN > maxMessageBytes-5 {
		t.Fatalf("boundary count %d no longer fits a single message; re-derive this test", maxN)
	}

	frame := make([]byte, 0, maxN+5)
	frame = append(frame, markerList32)
	frame = appendUint32(frame, uint32(maxN)) //nolint:gosec // G115: bounded by the message-cap guard above.
	frame = append(frame, bytes.Repeat([]byte{markerNull}, maxN)...)

	dec := packstream.NewDecoder(bytes.NewReader(frame))
	v, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue rejected a list exactly at the decoded-memory boundary: %v", err)
	}
	items, ok := v.([]packstream.Value)
	if !ok || len(items) != maxN {
		t.Fatalf("decoded %T of %d elements, want []Value of %d", v, len(items), maxN)
	}
}

// TestDecodedBudgetLegitimateTrafficDecodes is the behaviour-preservation
// regression: realistic large values — the shapes real drivers send as
// query parameters and bulk UNWIND batches — must keep decoding exactly as
// before. Amplification here is far below the budget, so none of these may
// be rejected.
func TestDecodedBudgetLegitimateTrafficDecodes(t *testing.T) {
	largeIntList := make([]packstream.Value, 200_000)
	for i := range largeIntList {
		largeIntList[i] = int64(i % 100)
	}
	wideMap := make(map[string]packstream.Value, 10_000)
	for i := range 10_000 {
		wideMap[string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('A'+i/260%26))+
			string(rune('a'+i/6760%26))] = int64(i)
	}
	batch := make([]packstream.Value, 2_000)
	for i := range batch {
		batch[i] = map[string]packstream.Value{
			"id":   int64(i),
			"name": "node",
			"ok":   true,
		}
	}

	cases := []struct {
		name string
		v    packstream.Value
	}{
		{"large_int_list", largeIntList},
		{"wide_map", wideMap},
		{"unwind_batch_of_maps", batch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			if err := enc.WriteValue(tc.v); err != nil {
				t.Fatalf("WriteValue: %v", err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
			got, err := dec.ReadValue()
			if err != nil {
				t.Fatalf("ReadValue rejected legitimate %s: %v", tc.name, err)
			}
			assertValueEqual(t, got, tc.v)
		})
	}
}

// TestChargeDecodedArithmetic asserts the budget bookkeeping directly —
// boundary acceptance, rejection one element past it, no budget consumption
// on a failed charge, cumulative accounting, and full restoration on Reset —
// without relying on process-global memory statistics (which are flaky under
// parallel and race-instrumented runs).
func TestChargeDecodedArithmetic(t *testing.T) {
	budget := packstream.MaxDecodedCollectionBytesForTest()
	elem := packstream.ListElemCostForTest()
	overhead := packstream.CollectionCostForTest()
	maxN := (budget - overhead) / elem

	t.Run("exact_boundary_accepted", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(nil))
		if err := dec.ChargeDecodedForTest("List", maxN, elem); err != nil {
			t.Fatalf("charge at exact boundary failed: %v", err)
		}
	})

	t.Run("one_past_boundary_rejected", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(nil))
		if err := dec.ChargeDecodedForTest("List", maxN+1, elem); !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
			t.Fatalf("charge past boundary error = %v, want ErrDecodedMemoryExceeded", err)
		}
	})

	t.Run("failed_charge_consumes_nothing", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(nil))
		if err := dec.ChargeDecodedForTest("List", maxN+1, elem); err == nil {
			t.Fatal("oversized charge unexpectedly succeeded")
		}
		if err := dec.ChargeDecodedForTest("List", maxN, elem); err != nil {
			t.Fatalf("budget was consumed by a failed charge: %v", err)
		}
	})

	t.Run("cumulative_across_charges", func(t *testing.T) {
		// Each charge is a third of the element budget — trivially within a
		// fresh budget on its own — so the third charge fails only because
		// the account is cumulative (including the per-collection overhead).
		dec := packstream.NewDecoder(bytes.NewReader(nil))
		third := maxN / 3
		if err := dec.ChargeDecodedForTest("List", third, elem); err != nil {
			t.Fatalf("first third-charge failed: %v", err)
		}
		if err := dec.ChargeDecodedForTest("List", third, elem); err != nil {
			t.Fatalf("second third-charge failed: %v", err)
		}
		if err := dec.ChargeDecodedForTest("List", third, elem); !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
			t.Fatalf("third third-charge error = %v, want ErrDecodedMemoryExceeded", err)
		}
	})

	t.Run("reset_restores_budget", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(nil))
		if err := dec.ChargeDecodedForTest("List", maxN, elem); err != nil {
			t.Fatalf("initial charge failed: %v", err)
		}
		if err := dec.ChargeDecodedForTest("List", maxN, elem); err == nil {
			t.Fatal("second charge on an exhausted budget unexpectedly succeeded")
		}
		dec.Reset(bytes.NewReader(nil))
		if err := dec.ChargeDecodedForTest("List", maxN, elem); err != nil {
			t.Fatalf("charge after Reset failed, budget was not restored: %v", err)
		}
	})
}
