package lpg

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestBagUintMatchesTheReferenceDecode pins the switch in bagUint against the
// zero-extending copy it replaced. The two must agree on every width and every
// value, because a disagreement is not a crash but a silently wrong property
// id, length or payload — read out of the middle of a record.
func TestBagUintMatchesTheReferenceDecode(t *testing.T) {
	// The reference is exactly the implementation bagUint had before the
	// switch: zero-extend n bytes into eight and decode little-endian.
	reference := func(buf []byte, off, n int) uint64 {
		var tmp [8]byte
		copy(tmp[:], buf[off:off+n])
		return binary.LittleEndian.Uint64(tmp[:])
	}

	values := []uint64{
		0, 1, 2, 127, 128, 255, 256, 65534, 65535, 65536,
		1 << 24, math.MaxUint32 - 1, math.MaxUint32, 1 << 32,
		math.MaxUint64 - 1, math.MaxUint64,
	}

	for _, sel := range []byte{0, 1, 2, 3} { // bagSizeOf: 1, 2, 4, 8
		n := bagSizeOf(sel)
		for _, v := range values {
			// A leading pad byte so off is never 0: an implementation that
			// ignored off would still pass at the start of a buffer.
			buf := append([]byte{0xAA}, bagPutUint(nil, v, n)...)
			got := bagUint(buf, 1, n)
			want := reference(buf, 1, n)
			if got != want {
				t.Fatalf("width %d, value %d: bagUint = %d, reference = %d", n, v, got, want)
			}
			// And the round trip: the low n bytes of v must come back.
			mask := uint64(math.MaxUint64)
			if n < 8 {
				mask = (uint64(1) << (8 * n)) - 1
			}
			if got != v&mask {
				t.Fatalf("width %d: round trip of %d gave %d, want %d", n, v, got, v&mask)
			}
		}
	}
}
