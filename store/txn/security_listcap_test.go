package txn

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSec_DecodeTxnListProp_HostileCountBounded is a regression lock-in for
// SEC-2026-06-14c (#1490, CWE-789): a crafted PropList payload declaring a
// 0xFFFFFFFF element-count followed by a truncated element body must fail-stop
// with a bounded error and must NOT drive a multi-gigabyte eager reservation.
//
// Before the fix, decodeTxnListProp did make([]lpg.PropertyValue, 0, count);
// lpg.PropertyValue is 24 bytes, so an unclamped count reserved ~103 GiB. The
// fix clamps the capacity hint to min(count, remaining/txnListElemMinBytes)
// (txnListCapHint), mirroring recovery.recoveryListCapHint and
// snapshot.listCapHint. The per-element loop already fails on the first
// truncated element, so the clamp only changes the eager reservation.
func TestSec_DecodeTxnListProp_HostileCountBounded(t *testing.T) {
	t.Parallel()

	// Wire: uint32 LE element-count = 0xFFFFFFFF, then a truncated element
	// header (1-byte kind + partial length) so the loop bails at index 0.
	buf := make([]byte, 0, 8)
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 0xFFFFFFFF)
	buf = append(buf, hdr[:]...)
	// One byte of element kind + 2 bytes (truncated 4-byte length) => the
	// loop sees len(buf) < 5 at index 0 and returns a truncated-header error.
	buf = append(buf, byte(lpg.PropInt64), 0x01, 0x02)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, _, err := decodeTxnListProp(buf)

	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected a bounded corruption error for a hostile element count, got nil")
	}

	// TotalAlloc is monotonic across the call; the clamp keeps the eager
	// reservation tiny (a handful of elements at most for this 3-byte body).
	// A 64 MiB ceiling is orders of magnitude below the ~103 GiB an unclamped
	// count would have reserved, yet far above this test's real footprint.
	const allocCeiling = 64 << 20
	if delta := after.TotalAlloc - before.TotalAlloc; delta > allocCeiling {
		t.Fatalf("decodeTxnListProp allocated %d bytes for a hostile count; want <= %d (clamp regressed)",
			delta, allocCeiling)
	}
}

// TestSec_TxnListCapHint_ClampsToRemaining asserts the bounding arithmetic
// directly: a hostile count is clamped to remaining/txnListElemMinBytes, while
// a legitimate small count passes through unchanged.
func TestSec_TxnListCapHint_ClampsToRemaining(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		count     uint32
		remaining int
		want      int
	}{
		{"hostile-count-clamped", 0xFFFFFFFF, 100, 100 / txnListElemMinBytes},
		{"legit-count-passthrough", 3, 1000, 3},
		{"zero-remaining", 0xFFFFFFFF, 0, 0},
		{"exact-fit", 10, 10 * txnListElemMinBytes, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := txnListCapHint(c.count, c.remaining); got != c.want {
				t.Fatalf("txnListCapHint(%d, %d) = %d, want %d", c.count, c.remaining, got, c.want)
			}
		})
	}
}

// TestSec_DecodeTxnListProp_LegitRoundtrip confirms the clamp does not break a
// legitimate list: a well-formed PropList still decodes to the exact elements.
func TestSec_DecodeTxnListProp_LegitRoundtrip(t *testing.T) {
	t.Parallel()
	orig := lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(7),
		lpg.StringValue("ok"),
		lpg.BoolValue(true),
	})
	buf := encodePropertyValue(nil, orig)
	got, rest, err := decodePropertyValue(buf)
	if err != nil {
		t.Fatalf("decodePropertyValue: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("decodePropertyValue left %d trailing bytes", len(rest))
	}
	if got.Kind() != lpg.PropList {
		t.Fatalf("Kind = %v, want PropList", got.Kind())
	}
	gotList, ok := got.List()
	if !ok {
		t.Fatal("List() returned !ok for a decoded PropList")
	}
	if len(gotList) != 3 {
		t.Fatalf("decoded list len = %d, want 3", len(gotList))
	}
	// Sanity: a header declaring 2 elements but with no element bodies must
	// still error, not silently succeed (the loop guards every element).
	if _, _, err := decodeTxnListProp([]byte{0x02, 0, 0, 0}); err == nil {
		t.Fatal("expected error for a list header with no element bodies")
	}
}
