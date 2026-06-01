package packstream_test

// T600: rapid-based round-trip tests for PackStream primitive types.
//
// Complements the table-driven tests in packstream_test.go by exercising the
// full int64 domain, float64 domain, arbitrary UTF-8 strings, and booleans
// with randomly generated inputs.
//
// Layer: short (no build tag required).

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// roundTripRapid encodes v and decodes it back, calling rt.Fatalf on any error.
func roundTripRapid(rt *rapid.T, v packstream.Value) packstream.Value {
	rt.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(v); err != nil {
		rt.Fatalf("WriteValue(%v): %v", v, err)
	}
	if err := enc.Flush(); err != nil {
		rt.Fatalf("Flush: %v", err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadValue()
	if err != nil {
		rt.Fatalf("ReadValue: %v", err)
	}
	return got
}

// TestPrimitivesRapid_Int exercises the full int64 domain via rapid.
// The encoder must select the tightest encoding (TinyInt, Int8, Int16, Int32,
// Int64) and the decoder must reconstruct the original value exactly.
func TestPrimitivesRapid_Int(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := rapid.Int64().Draw(rt, "v")
		got := roundTripRapid(rt, want)
		gv, ok := got.(int64)
		if !ok {
			rt.Fatalf("expected int64, got %T (%v)", got, got)
		}
		if gv != want {
			rt.Fatalf("int round-trip: want %d, got %d", want, gv)
		}
	})
}

// TestPrimitivesRapid_Float exercises the full float64 domain via rapid.
// NaN payloads are preserved: the decoded value must also be NaN.
// All other values must decode to the bit-identical IEEE-754 double.
func TestPrimitivesRapid_Float(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := rapid.Float64().Draw(rt, "v")
		got := roundTripRapid(rt, want)
		gf, ok := got.(float64)
		if !ok {
			rt.Fatalf("expected float64, got %T (%v)", got, got)
		}
		if math.IsNaN(want) {
			// NaN contract: the decoder must return a NaN payload.
			// The specific NaN bit-pattern is not guaranteed by PackStream;
			// any NaN satisfies the round-trip contract.
			if !math.IsNaN(gf) {
				rt.Fatalf("float round-trip: want NaN, got %v", gf)
			}
		} else {
			if gf != want {
				rt.Fatalf("float round-trip: want %v, got %v", want, gf)
			}
		}
	})
}

// TestPrimitivesRapid_Bool exercises both boolean values via rapid.
func TestPrimitivesRapid_Bool(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := rapid.Bool().Draw(rt, "v")
		got := roundTripRapid(rt, want)
		gv, ok := got.(bool)
		if !ok {
			rt.Fatalf("expected bool, got %T (%v)", got, got)
		}
		if gv != want {
			rt.Fatalf("bool round-trip: want %v, got %v", want, gv)
		}
	})
}

// TestPrimitivesRapid_Null verifies that nil encodes and decodes as nil.
func TestPrimitivesRapid_Null(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		got := roundTripRapid(rt, nil)
		if got != nil {
			rt.Fatalf("null round-trip: want nil, got %v", got)
		}
	})
}

// TestPrimitivesRapid_String exercises arbitrary valid UTF-8 strings via rapid.
// rapid.String() generates valid UTF-8; the PackStream Str8/Str16/Str32 markers
// are based on byte length, so multi-byte runes exercise boundary transitions
// at lower rune counts than single-byte ASCII.
func TestPrimitivesRapid_String(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		want := rapid.String().Draw(rt, "v")
		if !utf8.ValidString(want) {
			// rapid.String() is guaranteed valid UTF-8; this guard is belt-and-
			// suspenders — if it ever fails, the test data generator is broken.
			rt.Skip()
		}
		got := roundTripRapid(rt, want)
		gv, ok := got.(string)
		if !ok {
			rt.Fatalf("expected string, got %T (%v)", got, got)
		}
		if gv != want {
			rt.Fatalf("string round-trip: want %q, got %q", want, gv)
		}
	})
}

// TestStringBoundaries_RoundTrip verifies that the encoding marker boundaries
// for strings are handled correctly: TinyStr (≤15 bytes), Str8 (16–255),
// Str16 (256–65535), Str32 (65536+).
//
// A length-0 string and the exact byte-length boundary at each marker
// transition are tested. The 65535-byte and 65536-byte cases are the
// Str16/Str32 boundary.
func TestStringBoundaries_RoundTrip(t *testing.T) {
	boundaries := []struct {
		name   string
		length int
	}{
		{"len_0", 0},
		{"len_15_tinystr_max", 15},
		{"len_16_str8_start", 16},
		{"len_255_str8_max", 255},
		{"len_256_str16_start", 256},
		{"len_65535_str16_max", 65535},
		{"len_65536_str32_start", 65536},
	}
	for _, tc := range boundaries {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Repeat("a", tc.length)
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			if err := enc.WriteString(want); err != nil {
				t.Fatalf("WriteString(len=%d): %v", tc.length, err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			dec := packstream.NewDecoder(&buf)
			got, err := dec.ReadString()
			if err != nil {
				t.Fatalf("ReadString: %v", err)
			}
			if got != want {
				t.Errorf("len=%d: round-trip mismatch (byte lengths: want %d, got %d)",
					tc.length, len(want), len(got))
			}
		})
	}
}
