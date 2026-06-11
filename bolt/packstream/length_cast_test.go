package packstream_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// These tests are the regression gate for the 32-bit length-cast bypass
// (rmp #1352, 2026-06-10 audit). PackStream length/count prefixes are
// unsigned 32-bit values, but the decoder sizes its allocations with Go
// ints: the vulnerable code converted first — int(binary.BigEndian.Uint32)
// — so on a 32-bit platform a prefix in [2^31, 2^32) wrapped to a NEGATIVE
// int, slipped past every `n > budget` guard (a negative n compares below
// any budget), and panicked the subsequent make([]byte, n) /
// make([]Value, n) with "len out of range". The fix validates the prefix
// while it is still unsigned, rejecting anything above MaxInt32 with
// ErrLengthExceedsInput before any conversion, budget charge, or
// allocation, so decode behaviour is identical on 32- and 64-bit builds.
//
// On a 64-bit platform the conversion cannot wrap, so the gate isolates the
// pre-conversion check the only way it is observable here: it lifts the wire
// byte budget out of the way (SetUnboundedBudgetForTest). Without the fix a
// wrap-range prefix then does NOT produce ErrLengthExceedsInput — Bytes and
// String commit the multi-GiB make() and fail on the short read with an I/O
// error, while List and Map fall through to the decoded-memory budget and
// fail with ErrDecodedMemoryExceeded. With the fix, every arm reports
// ErrLengthExceedsInput without allocating.

// wrap32Frame builds a 5-byte frame: one marker byte followed by a uint32
// big-endian length/count prefix.
func wrap32Frame(marker byte, length uint32) []byte {
	return appendUint32([]byte{marker}, length)
}

// wrap32Cases enumerates the four decoder arms that read a uint32
// length/count prefix from the wire. markerBytes32 et al. are declared in
// length_bound_test.go (same package).
var wrap32Cases = []struct {
	name   string
	marker byte
	read   func(*packstream.Decoder) error
}{
	{"Bytes", markerBytes32, func(d *packstream.Decoder) error { _, err := d.ReadBytes(); return err }},
	{"String", markerStr32, func(d *packstream.Decoder) error { _, err := d.ReadString(); return err }},
	{"List", markerList32, func(d *packstream.Decoder) error { _, err := d.ReadListHeader(); return err }},
	{"Map", markerMap32, func(d *packstream.Decoder) error { _, err := d.ReadMapHeader(); return err }},
}

// TestWire32LengthPrefixWrapRejectedBeforeCast asserts that every wire
// 32-bit length/count prefix in the wrap range [2^31, 2^32) is rejected
// with ErrLengthExceedsInput while still unsigned — before the int
// conversion that would go negative on a 32-bit platform, before the
// decoded-memory charge, and before any make().
func TestWire32LengthPrefixWrapRejectedBeforeCast(t *testing.T) {
	// 0x80000000 is the smallest prefix that wraps a 32-bit int;
	// 0xFFFFFFFF is the largest encodable prefix (wraps to -1).
	for _, length := range []uint32{0x80000000, 0xFFFFFFFF} {
		for _, tc := range wrap32Cases {
			t.Run(fmt.Sprintf("%s_0x%08X", tc.name, length), func(t *testing.T) {
				dec := packstream.NewDecoder(bytes.NewReader(wrap32Frame(tc.marker, length)))
				dec.SetUnboundedBudgetForTest()
				err := tc.read(dec)
				if !errors.Is(err, packstream.ErrLengthExceedsInput) {
					t.Fatalf("%s prefix 0x%08X: error = %v, want ErrLengthExceedsInput before the int conversion",
						tc.name, length, err)
				}
			})
		}
	}
}

// TestWire32LengthPrefixBelowWrapReachesBudgets pins the boundary and the
// check ordering: a prefix of exactly MaxInt32 (0x7FFFFFFF, the largest
// value that cannot wrap) must NOT be rejected by the pre-conversion cap.
// With the byte budget lifted, a List count of MaxInt32 falls through to the
// decoded-memory budget, which rejects it — proving the cap does not
// over-reject and that it composes with, rather than replaces, the existing
// budget machinery.
func TestWire32LengthPrefixBelowWrapReachesBudgets(t *testing.T) {
	dec := packstream.NewDecoder(bytes.NewReader(wrap32Frame(markerList32, 0x7FFFFFFF)))
	dec.SetUnboundedBudgetForTest()
	_, err := dec.ReadListHeader()
	if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
		t.Fatalf("List count 0x7FFFFFFF: error = %v, want ErrDecodedMemoryExceeded from the decoded-memory budget", err)
	}
}
