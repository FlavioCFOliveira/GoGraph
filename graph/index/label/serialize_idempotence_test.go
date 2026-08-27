package label

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// serialize_idempotence_test.go — regression cover for #2609.
//
// A Serialize/Deserialize/Serialize cycle used to change the BYTES of a label
// built by AddRange whose width fell in the band where the reader down-converts
// it: measured 55 B in and 64, 66, 68, 70 or 72 B out at widths 4 through 8, a
// fixpoint only after one full cycle. No content was ever lost — membership is
// identical across the cycle — but a snapshot of unchanged data was not
// byte-reproducible, which defeats fixture diffing and any content-addressed
// comparison of images.
//
// The cause is that AddRange builds a RUN container on the bitmap tier, while
// index.NodeSetFromBitmap moves a bitmap of at most smallSetMax ids back to the
// inline tier on the way in, where it re-materialises through AddMany as an
// ARRAY container. Run and array encode the same ids in different bytes.
//
// The repair normalises the container before WriteTo, bounded to sets of at most
// smallSetMax ids so the copy it needs is free (#2609). Widths above the bound
// are unaffected and were already stable.

const idemSmallMax = 8

func idemSerialize(t *testing.T, i *Index) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := i.Serialize(&b); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return b.Bytes()
}

func idemRoundTrip(t *testing.T, img []byte) *Index {
	t.Helper()
	out := NewIndex()
	if err := out.Deserialize(bytes.NewReader(img)); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	return out
}

func idemBuildRange(w int) *Index {
	i := NewIndex()
	i.AddRange(1, 1000, graph.NodeID(1000+w-1))
	return i
}

func idemBuildAdd(w int) *Index {
	i := NewIndex()
	for k := 0; k < w; k++ {
		i.Add(1, graph.NodeID(1000+k))
	}
	return i
}

func idemMembers(w int) []graph.NodeID {
	out := make([]graph.NodeID, 0, w)
	for k := 0; k < w; k++ {
		out = append(out, graph.NodeID(1000+k))
	}
	return out
}

// TestSerializeIsIdempotentAcrossTheDownConversionBand sweeps the whole band,
// including the widths that were already stable, so a failure localises.
func TestSerializeIsIdempotentAcrossTheDownConversionBand(t *testing.T) {
	for w := 1; w <= 16; w++ {
		t.Run(widthName(w), func(t *testing.T) {
			idx := idemBuildRange(w)
			first := idemSerialize(t, idx)
			second := idemSerialize(t, idemRoundTrip(t, first))
			third := idemSerialize(t, idemRoundTrip(t, second))

			if !bytes.Equal(first, second) {
				t.Errorf("a Serialize/Deserialize cycle changed the image from %d to %d bytes; "+
					"an unchanged logical state must be byte-reproducible",
					len(first), len(second))
			}
			if !bytes.Equal(second, third) {
				t.Errorf("the image is not a fixpoint: %d then %d bytes",
					len(second), len(third))
			}
		})
	}
}

// TestSerializeMembershipSurvivesTheCycle asserts the content separately, so
// "the bytes did not change" can never be satisfied by a cycle that lost ids.
func TestSerializeMembershipSurvivesTheCycle(t *testing.T) {
	for w := 1; w <= 16; w++ {
		t.Run(widthName(w), func(t *testing.T) {
			want := idemMembers(w)
			back := idemRoundTrip(t, idemSerialize(t, idemBuildRange(w)))

			if got := back.Count(1); got != uint64(len(want)) {
				t.Fatalf("Count = %d, want %d", got, len(want))
			}
			got := back.Scan(1)
			if len(got) != len(want) {
				t.Fatalf("Scan returned %d ids, want %d", len(got), len(want))
			}
			for k := range want {
				if got[k] != want[k] {
					t.Fatalf("Scan[%d] = %d, want %d", k, got[k], want[k])
				}
			}
		})
	}
}

// TestSerializeIsAFunctionOfContentsWithinTheBound asserts the second half: up
// to smallSetMax ids, the image depends on the logical contents and not on how
// the label was built. Above the bound the two forms still differ, deliberately
// and stably — that difference is a measurement the module records rather than a
// defect, and the control below pins it so the bound cannot drift unnoticed.
func TestSerializeIsAFunctionOfContentsWithinTheBound(t *testing.T) {
	for w := 1; w <= idemSmallMax; w++ {
		t.Run(widthName(w), func(t *testing.T) {
			byRange := idemSerialize(t, idemBuildRange(w))
			byAdd := idemSerialize(t, idemBuildAdd(w))
			if !bytes.Equal(byRange, byAdd) {
				t.Errorf("the same %d ids serialize to %d bytes when built by AddRange and %d when "+
					"built by Add; within the bound the image must be a function of the contents",
					w, len(byRange), len(byAdd))
			}
		})
	}

	t.Run("control: above the bound the two forms still differ", func(t *testing.T) {
		const w = idemSmallMax + 1
		byRange := idemSerialize(t, idemBuildRange(w))
		byAdd := idemSerialize(t, idemBuildAdd(w))
		if bytes.Equal(byRange, byAdd) {
			t.Errorf("AddRange and Add produced identical images at width %d; the normalisation is "+
				"bounded at %d ids, so reaching above it means the bound moved and the cost "+
				"measurement behind it no longer applies", w, idemSmallMax)
		}
	})
}

func widthName(w int) string {
	return "width " + string(rune('0'+w/10)) + string(rune('0'+w%10))
}
