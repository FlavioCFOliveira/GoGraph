package snapshot

// Security fix-regression battery — integer-overflow / wrap surfaces in the
// new bounding helpers (capHint, listCapHint, weightsByteLen) and the
// decodeListPropertyValue element loop.
//
// Hypotheses #2 and #5 from the audit: any uint64->int truncation, negative
// wrap, or unbounded eager reservation in the size math the fix introduced.
// These are bounded unit proofs — no large allocations — that establish the
// helpers behave (or do not) at the extremes.

import (
	"bytes"
	"math"
	"testing"
)

// TestSec_FixReg_CapHintExtremes pins capHint at the boundary and extreme
// inputs the decoders can feed it (count up to the 1<<40 ceiling; maxCap is
// always a small positive constant). On a 64-bit platform int is 64-bit so
// neither uint64(maxCap) nor int(count<maxCap) truncates. This is the robust
// case; the test documents it so a future 32-bit port or a maxCap change is
// caught.
func TestSec_FixReg_CapHintExtremes(t *testing.T) {
	cases := []struct {
		name   string
		count  uint64
		maxCap int
		want   int
	}{
		{"zero", 0, 1 << 20, 0},
		{"below-clamp", 12345, 1 << 20, 12345},
		{"at-clamp", 1 << 20, 1 << 20, 1 << 20}, // count == maxCap -> not < -> returns maxCap
		{"one-below-clamp", (1 << 20) - 1, 1 << 20, (1 << 20) - 1},
		{"record-ceiling", 1 << 40, 1 << 20, 1 << 20}, // hostile-but-under-ceiling -> clamp
		{"near-max-u64", math.MaxUint64, 1 << 20, 1 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capHint(tc.count, tc.maxCap)
			if got != tc.want {
				t.Fatalf("capHint(%d, %d) = %d; want %d", tc.count, tc.maxCap, got, tc.want)
			}
			if got < 0 {
				t.Fatalf("capHint(%d, %d) returned a negative cap %d (would panic make())", tc.count, tc.maxCap, got)
			}
		})
	}
}

// TestSec_FixReg_ListCapHintExtremes pins listCapHint: count is an untrusted
// uint32 (up to ~4.29e9), remaining is len(raw) >= 0. The hint must never
// exceed remaining/5 and never go negative. A maliciously huge count with a
// tiny remaining must clamp to remaining/5, not to count.
func TestSec_FixReg_ListCapHintExtremes(t *testing.T) {
	cases := []struct {
		name      string
		count     uint32
		remaining int
		want      int
	}{
		{"zero-zero", 0, 0, 0},
		{"small-fits", 3, 100, 3},
		{"count-exceeds-remaining", math.MaxUint32, 4, 0},     // 4/5 = 0 elements possible
		{"count-exceeds-remaining-2", math.MaxUint32, 50, 10}, // 50/5 = 10
		{"exact", 10, 50, 10},
		{"remaining-zero-count-huge", math.MaxUint32, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listCapHint(tc.count, tc.remaining)
			if got != tc.want {
				t.Fatalf("listCapHint(%d, %d) = %d; want %d", tc.count, tc.remaining, got, tc.want)
			}
			if got < 0 {
				t.Fatalf("listCapHint(%d, %d) returned negative %d", tc.count, tc.remaining, got)
			}
		})
	}
}

// TestSec_FixReg_ListElementAmplification attacks decodeListPropertyValue
// (hypothesis #5): a PropList value whose declared element-count is huge but
// whose body is short. listCapHint clamps the eager make() to remaining/5, and
// the per-element loop validates each element's header (kind+len) and body
// against the remaining bytes, so a truncated element header / body must abort
// with a corruption error and bounded allocation — never a giant reservation.
//
// We feed a 4-byte count of MaxUint32 followed by zero element bytes: the loop
// must fail on the first "truncated element header" check, not pre-allocate
// 4.29e9 PropertyValues (~100+ GiB). The make() hint is clamped to len(raw)/5 =
// 0, so nothing large is reserved.
func TestSec_FixReg_ListElementAmplification(t *testing.T) {
	var raw []byte
	raw = secStorePutU32(raw, math.MaxUint32) // hostile element count
	// no element bytes follow
	_, err := decodeListPropertyValue(raw)
	if err == nil {
		t.Fatal("decodeListPropertyValue accepted MaxUint32 elements with an empty body; want corruption error")
	}
	// A second case: declare a huge count, give one valid element, then stop.
	var raw2 []byte
	raw2 = secStorePutU32(raw2, 1000000) // 1e6 declared
	raw2 = append(raw2, byte(4))         // elem kind PropBool
	raw2 = secStorePutU32(raw2, 1)       // elem valueLen 1
	raw2 = append(raw2, 0x01)            // elem value
	// only one element actually present; loop must fail on element index 1.
	_, err2 := decodeListPropertyValue(raw2)
	if err2 == nil {
		t.Fatal("decodeListPropertyValue accepted 1e6 declared elements with one present; want corruption error")
	}
}

// TestSec_FixReg_WeightsByteLenOverflow exercises weightsByteLen at the
// overflow boundary: wsize is a 0-255 byte, nE a full uint64. bits.Mul64 must
// surface any product that overflows uint64 (hi != 0) or exceeds maxInt, and
// the manifest-budget bound must reject a product larger than the file could
// hold. None of these may panic or silently truncate to a short buffer.
func TestSec_FixReg_WeightsByteLenOverflow(t *testing.T) {
	// nE * wsize overflows uint64 high word: wsize=8, nE just over 1<<61.
	if _, err := weightsByteLen(8, (uint64(1)<<61)+1, -1, uint64(8*maxCSRCount)); err == nil {
		t.Fatal("weightsByteLen accepted an nE*wsize product overflowing uint64; want corruption error")
	}
	// Product within uint64 but exceeding maxInt (only meaningful where it can
	// exceed maxInt; on 64-bit maxInt is 1<<63-1 so use wsize=8, nE>1<<60).
	if _, err := weightsByteLen(8, (uint64(1)<<60)+1, -1, uint64(8*maxCSRCount)); err == nil {
		// 8 * (2^60+1) = 2^63+8 which exceeds maxInt(2^63-1) -> must reject.
		t.Fatal("weightsByteLen accepted a product exceeding maxInt; want corruption error")
	}
	// Product within range but exceeding the precise manifest byte budget.
	if _, err := weightsByteLen(8, 1000, 100 /*maxBytes*/, 100 /*byteBudget*/); err == nil {
		t.Fatal("weightsByteLen accepted 8000 weight bytes against a 100-byte file budget; want corruption error")
	}
	// Legitimate small case must succeed and return the exact byte length.
	n, err := weightsByteLen(8, 10, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("weightsByteLen rejected a legitimate small case: %v", err)
	}
	if n != 80 {
		t.Fatalf("weightsByteLen(8,10,...) = %d; want 80", n)
	}
}

// TestSec_FixReg_EdgeHandlesNestedCount is a bounded sanity check that the
// edgehandles per-record label/prop counts (uint32, bounded to
// edgeHandlesMaxCount) cannot drive a giant reservation when the body is short:
// a record declaring a vast labelCount with no label-index bytes must fail on
// the first index read, not pre-allocate. ReadEdgeHandles appends labels one at
// a time (no eager make on labelCount), so this is robust by construction; the
// test pins it.
func TestSec_FixReg_EdgeHandlesNestedCount(t *testing.T) {
	var b []byte
	b = secStorePutU32(b, edgeHandlesMagic)
	b = secStorePutU32(b, edgeHandlesFormatVersion)
	b = secStorePutU64(b, 0) // empty label table
	b = secStorePutU64(b, 0) // empty key table
	b = secStorePutU64(b, 1) // one record
	// record: src, dst, handle, then a hostile labelCount with no index bytes.
	b = secStorePutU64(b, 1)              // src
	b = secStorePutU64(b, 2)              // dst
	b = secStorePutU64(b, 3)              // handle
	b = secStorePutU32(b, math.MaxUint32) // hostile labelCount, no bytes follow
	if _, err := ReadEdgeHandles(bytes.NewReader(b)); err == nil {
		t.Fatal("ReadEdgeHandles accepted a record declaring MaxUint32 labels with no body; want corruption error")
	}
}
