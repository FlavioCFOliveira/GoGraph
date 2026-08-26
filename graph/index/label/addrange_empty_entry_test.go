package label

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// addrange_empty_entry_test.go — regression cover for #2608.
//
// AddRange used to store its NodeSet back unconditionally, and NodeSet.AddRange
// promoted to the bitmap tier before it looked at the interval. An interval
// naming no ids therefore left a bitmap behind and the store-back minted a map
// entry for a label that carries nothing. The entry was permanent, reached the
// serialized image at 20 bytes apiece, and was invisible through Count, Scan and
// Has — so nothing but the image size could see it.
//
// RemoveRange promises the opposite for its own direction ("Empty bitmaps are
// deleted so the map does not grow unboundedly after bulk-remove operations")
// and always kept that promise. AddRange now behaves the same way.

// serializedLabelCount reads the labelCount field out of a serialized image:
// magic (uint32) and version (uint32) precede it.
func serializedLabelCount(t *testing.T, img []byte) uint32 {
	t.Helper()
	const off = 8
	if len(img) < off+4 {
		t.Fatalf("image of %d bytes is too short to hold a labelCount", len(img))
	}
	return binary.LittleEndian.Uint32(img[off : off+4])
}

func serializeIndex(t *testing.T, i *Index) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := i.Serialize(&b); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return b.Bytes()
}

func TestAddRangeEmptyIntervalMintsNoEntry(t *testing.T) {
	empty := serializeIndex(t, NewIndex())
	if got := serializedLabelCount(t, empty); got != 0 {
		t.Fatalf("the empty index declares %d labels, want 0", got)
	}

	tests := []struct {
		name     string
		from, to graph.NodeID
	}{
		{"inverted by a wide margin", 100, 50},
		{"inverted by one", 51, 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const labels = 1000
			idx := NewIndex()
			for l := uint32(0); l < labels; l++ {
				idx.AddRange(l, tt.from, tt.to)
			}

			// Invisible through every query path, as it always was.
			for l := uint32(0); l < labels; l++ {
				if got := idx.Count(l); got != 0 {
					t.Fatalf("Count(%d) = %d, want 0", l, got)
				}
				if idx.Scan(l) != nil {
					t.Fatalf("Scan(%d) returned ids for a label that carries none", l)
				}
			}

			// And now invisible in the image too.
			img := serializeIndex(t, idx)
			if got := serializedLabelCount(t, img); got != 0 {
				t.Errorf("%d empty-interval AddRange calls left an image declaring %d labels, want 0",
					labels, got)
			}
			if !bytes.Equal(img, empty) {
				t.Errorf("the image is %d bytes against %d for an empty index; an interval naming "+
					"no ids must cost nothing on disk", len(img), len(empty))
			}
		})
	}
}

// TestAddRangeNonEmptyIntervalStillStores is the control: the repair must not
// reach a range that names at least one id.
func TestAddRangeNonEmptyIntervalStillStores(t *testing.T) {
	idx := NewIndex()
	idx.AddRange(7, 100, 104)

	if got, want := idx.Count(7), uint64(5); got != want {
		t.Fatalf("Count = %d, want %d", got, want)
	}
	if got := idx.Scan(7); len(got) != 5 {
		t.Fatalf("Scan returned %d ids, want 5", len(got))
	}
	if got := serializedLabelCount(t, serializeIndex(t, idx)); got != 1 {
		t.Errorf("the image declares %d labels, want 1", got)
	}

	// A degenerate but non-empty interval (from == to) names exactly one id and
	// must be stored.
	idx.AddRange(8, 200, 200)
	if got, want := idx.Count(8), uint64(1); got != want {
		t.Fatalf("Count of the single-id range = %d, want %d", got, want)
	}
	if got := serializedLabelCount(t, serializeIndex(t, idx)); got != 2 {
		t.Errorf("the image declares %d labels, want 2", got)
	}
}
