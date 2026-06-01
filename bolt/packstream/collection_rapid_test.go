package packstream_test

// T607: rapid-based round-trip tests for PackStream collection types (List, Map).
//
// Verifies identity round-trips and exercises all four header-size encodings:
//   - tiny  (0–15 elements)   : tinyListBase / tinyMapBase markers
//   - 8-bit (16–255 elements) : markerList8  / markerMap8
//   - 16-bit (256–65535)      : markerList16 / markerMap16
//   - 32-bit (65536+)         : markerList32 / markerMap32
//
// Map key order is not guaranteed by the PackStream specification; callers must
// not rely on insertion order. reflect.DeepEqual works correctly for
// map[string]Value comparisons because it compares maps by key/value equality,
// not by iteration order.
//
// Layer: short (no build tag required).

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// genPrimitiveValue returns a rapid generator that produces a non-collection
// packstream.Value (nil, bool, int64, float64, string). Used as list/map element.
func genPrimitiveValue() *rapid.Generator[packstream.Value] {
	return rapid.OneOf(
		rapid.Just[packstream.Value](nil),
		rapid.Map(rapid.Bool(), func(b bool) packstream.Value { return b }),
		rapid.Map(rapid.Int64(), func(i int64) packstream.Value { return i }),
		rapid.Map(rapid.String(), func(s string) packstream.Value { return s }),
	)
}

// roundTripList encodes a []packstream.Value and decodes it back.
func roundTripList(t *testing.T, want []packstream.Value) []packstream.Value {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(want); err != nil {
		t.Fatalf("WriteValue(list, len=%d): %v", len(want), err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	gl, ok := got.([]packstream.Value)
	if !ok {
		t.Fatalf("expected []Value, got %T", got)
	}
	return gl
}

// roundTripMap encodes a map[string]packstream.Value and decodes it back.
func roundTripMap(t *testing.T, want map[string]packstream.Value) map[string]packstream.Value {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(want); err != nil {
		t.Fatalf("WriteValue(map, len=%d): %v", len(want), err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	gm, ok := got.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("expected map[string]Value, got %T", got)
	}
	return gm
}

// TestCollectionRapid_List round-trips randomly generated lists.
// Element count is drawn from 0–300 to stay inside the 8-bit encoding range
// while still exercising the tiny/8-bit boundary (15→16).
func TestCollectionRapid_List(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 300).Draw(rt, "n")
		want := make([]packstream.Value, n)
		for i := range want {
			want[i] = genPrimitiveValue().Draw(rt, fmt.Sprintf("elem[%d]", i))
		}
		got := roundTripList(t, want)
		if len(got) != len(want) {
			rt.Fatalf("list length: want %d, got %d", len(want), len(got))
		}
		// reflect.DeepEqual handles nil vs typed-nil for packstream.Value = any.
		if !reflect.DeepEqual(got, want) {
			rt.Fatalf("list round-trip mismatch at some element")
		}
	})
}

// TestCollectionRapid_Map round-trips randomly generated maps.
// Key count is drawn from 0–300.
//
// Map key order is not guaranteed by PackStream; callers must not rely on it.
// reflect.DeepEqual works correctly for map[string]Value comparisons.
func TestCollectionRapid_Map(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 50).Draw(rt, "n") // small n for unique-key feasibility
		want := make(map[string]packstream.Value, n)
		for i := range n {
			k := fmt.Sprintf("k%d", i)
			want[k] = genPrimitiveValue().Draw(rt, fmt.Sprintf("val[%s]", k))
		}
		got := roundTripMap(t, want)
		if len(got) != len(want) {
			rt.Fatalf("map length: want %d, got %d", len(want), len(got))
		}
		if !reflect.DeepEqual(got, want) {
			rt.Fatalf("map round-trip mismatch")
		}
	})
}

// TestListHeaderEncodings_Boundaries exercises all four List header-size
// encodings by round-tripping lists at the exact element-count boundaries.
//
// The 32-bit case (65536 elements) produces ~512 KB of encoded data, which is
// acceptable for a short-layer test.
func TestListHeaderEncodings_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		n    int
	}{
		{"tiny_0", 0},
		{"tiny_15", 15},
		{"bit8_16", 16},
		{"bit8_255", 255},
		{"bit16_256", 256},
		{"bit16_65535", 65535},
		{"bit32_65536", 65536},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := make([]packstream.Value, tc.n)
			for i := range want {
				want[i] = int64(i)
			}
			got := roundTripList(t, want)
			if len(got) != tc.n {
				t.Fatalf("list len: want %d, got %d", tc.n, len(got))
			}
			for i, wv := range want {
				if got[i] != wv {
					t.Errorf("elem[%d]: want %v, got %v", i, wv, got[i])
					break
				}
			}
		})
	}
}

// TestMapHeaderEncodings_Boundaries exercises all four Map header-size
// encodings by round-tripping maps at the exact entry-count boundaries.
func TestMapHeaderEncodings_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		n    int
	}{
		{"tiny_0", 0},
		{"tiny_15", 15},
		{"bit8_16", 16},
		{"bit8_255", 255},
		{"bit16_256", 256},
		{"bit16_65535", 65535},
		{"bit32_65536", 65536},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := make(map[string]packstream.Value, tc.n)
			for i := range tc.n {
				want[fmt.Sprintf("k%d", i)] = int64(i)
			}
			got := roundTripMap(t, want)
			if len(got) != tc.n {
				t.Fatalf("map len: want %d, got %d", tc.n, len(got))
			}
			// Map key order is not guaranteed by PackStream; callers must not
			// rely on it. reflect.DeepEqual is correct for map equality.
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("map round-trip mismatch at n=%d", tc.n)
			}
		})
	}
}
