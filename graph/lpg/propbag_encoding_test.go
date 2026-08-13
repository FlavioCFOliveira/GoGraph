package lpg

import (
	"math"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// propbag_encoding_test.go — the round-trip contract of the byte-stream tier
// introduced in sprint 339 (#2408).
//
// The tier encodes four scalar kinds into a self-describing byte stream with
// variable-width property ids and payloads. That is a lot of branches on width
// selectors, and the failure mode of getting one wrong is a value that comes
// back subtly different rather than a crash — a -0.0 that reads as 0.0, a
// MinInt64 that wraps, a string whose length field truncated. These tests exist
// to make every such case loud.

// bagCases is the table of values whose encoding has an edge to fall off.
func bagCases() []struct {
	name string
	val  PropertyValue
} {
	return []struct {
		name string
		val  PropertyValue
	}{
		{"int_zero", Int64Value(0)},
		{"int_one", Int64Value(1)},
		{"int_minus_one", Int64Value(-1)},
		{"int_127", Int64Value(127)},
		{"int_128", Int64Value(128)},
		{"int_255", Int64Value(255)},
		{"int_256", Int64Value(256)},
		{"int_65535", Int64Value(65535)},
		{"int_65536", Int64Value(65536)},
		{"int_maxint32", Int64Value(math.MaxInt32)},
		{"int_maxint64", Int64Value(math.MaxInt64)},
		{"int_minint64", Int64Value(math.MinInt64)},
		{"float_zero", Float64Value(0)},
		{"float_negzero", Float64Value(math.Copysign(0, -1))},
		{"float_nan", Float64Value(math.NaN())},
		{"float_inf", Float64Value(math.Inf(1))},
		{"float_neginf", Float64Value(math.Inf(-1))},
		{"float_smallest", Float64Value(math.SmallestNonzeroFloat64)},
		{"float_max", Float64Value(math.MaxFloat64)},
		{"bool_true", BoolValue(true)},
		{"bool_false", BoolValue(false)},
		{"string_empty", StringValue("")},
		{"string_one", StringValue("x")},
		{"string_255", StringValue(strings.Repeat("a", 255))},
		{"string_256", StringValue(strings.Repeat("b", 256))},
		{"string_65536", StringValue(strings.Repeat("c", 65536))},
		{"string_utf8", StringValue("héllo wörld ✓ 日本語")},
		{"string_nul", StringValue("a\x00b")},
		// Temporals are carried through the property layer as tagged strings
		// (\x01..\x06), so a string containing those bytes must survive the
		// encoding untouched.
		{"string_tagged_temporal", StringValue("\x01" + "2026-08-11")},
	}
}

// sameValue compares two PropertyValues bit-exactly, which ordinary equality
// does not do for floats: NaN != NaN, and -0.0 == 0.0 would let a sign-losing
// encoder pass.
func sameValue(t *testing.T, got, want PropertyValue) bool {
	t.Helper()
	if got.Kind() != want.Kind() {
		return false
	}
	switch want.Kind() {
	case PropFloat64:
		g, _ := got.Float64()
		w, _ := want.Float64()
		return math.Float64bits(g) == math.Float64bits(w)
	case PropInt64:
		g, _ := got.Int64()
		w, _ := want.Int64()
		return g == w
	case PropBool:
		g, _ := got.Bool()
		w, _ := want.Bool()
		return g == w
	case PropString:
		g, _ := got.String()
		w, _ := want.String()
		return g == w
	}
	return false
}

// TestPropBagEncoding_RoundTripsEveryEdgeCase stores each value under several
// property-key widths, because the key's width selector and the payload's share
// one metadata byte and a shift error in either corrupts the other.
func TestPropBagEncoding_RoundTripsEveryEdgeCase(t *testing.T) {
	keys := []PropertyKeyID{0, 1, 255, 256, 65535, 65536, math.MaxUint32}
	for _, tc := range bagCases() {
		for _, k := range keys {
			var b propBag
			b.set(k, tc.val)
			if b.m != nil {
				t.Fatalf("%s key=%d: promoted to the map tier; this kind should stream", tc.name, k)
			}
			got, ok := b.get(k)
			if !ok {
				t.Fatalf("%s key=%d: get reported absent", tc.name, k)
			}
			if !sameValue(t, got, tc.val) {
				t.Fatalf("%s key=%d: round-trip mismatch: got %#v want %#v", tc.name, k, got, tc.val)
			}
			if b.len() != 1 {
				t.Fatalf("%s key=%d: len = %d, want 1", tc.name, k, b.len())
			}
		}
	}
}

// TestPropBagEncoding_NonStreamableKindsPromote pins that the kinds the stream
// does not model reach the map tier rather than being silently dropped or
// mis-encoded.
func TestPropBagEncoding_NonStreamableKindsPromote(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  PropertyValue
	}{
		{"time", TimeValue(time.Unix(1_700_000_000, 123).UTC())},
		{"bytes", BytesValue([]byte{1, 2, 3})},
		{"list", ListValue([]PropertyValue{Int64Value(1), StringValue("a")})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b propBag
			b.set(1, StringValue("streamed-first"))
			b.set(2, tc.val)
			if b.m == nil {
				t.Fatal("storing a non-streamable kind did not promote to the map tier")
			}
			// The record written before the promotion must survive it.
			if got, ok := b.get(1); !ok || !sameValue(t, got, StringValue("streamed-first")) {
				t.Fatalf("pre-promotion record lost: got %#v ok=%v", got, ok)
			}
			got, ok := b.get(2)
			if !ok || got.Kind() != tc.val.Kind() {
				t.Fatalf("get(2) = %#v, %v; want kind %v", got, ok, tc.val.Kind())
			}
		})
	}
}

// TestPropBagEncoding_MultiRecordSequences exercises the scan: several records
// of mixed kinds and widths in one buffer, then overwrite and delete in the
// middle, which are the operations that rebuild it.
func TestPropBagEncoding_MultiRecordSequences(t *testing.T) {
	var b propBag
	want := map[PropertyKeyID]PropertyValue{
		1:   StringValue("first"),
		2:   Int64Value(-5),
		300: Float64Value(math.Pi),
		4:   BoolValue(true),
		5:   StringValue(""),
	}
	for k, v := range want {
		b.set(k, v)
	}
	for k, v := range want {
		got, ok := b.get(k)
		if !ok || !sameValue(t, got, v) {
			t.Fatalf("key %d: got %#v ok=%v, want %#v", k, got, ok, v)
		}
	}
	if b.len() != len(want) {
		t.Fatalf("len = %d, want %d", b.len(), len(want))
	}

	// Overwrite a record in the middle with one of a DIFFERENT width, which is
	// what makes the rebuild non-trivial.
	b.set(2, StringValue(strings.Repeat("z", 300)))
	want[2] = StringValue(strings.Repeat("z", 300))
	if got, ok := b.get(2); !ok || !sameValue(t, got, want[2]) {
		t.Fatalf("after widening overwrite: got %#v ok=%v", got, ok)
	}
	if b.len() != len(want) {
		t.Fatalf("len after overwrite = %d, want %d — an overwrite must not duplicate", b.len(), len(want))
	}
	for k, v := range want {
		if got, ok := b.get(k); !ok || !sameValue(t, got, v) {
			t.Fatalf("key %d disturbed by the overwrite: got %#v ok=%v, want %#v", k, got, ok, v)
		}
	}

	// Delete from the middle; every sibling must survive.
	if empty := b.del(300); empty {
		t.Fatal("del reported the bag empty while four records remain")
	}
	delete(want, 300)
	if _, ok := b.get(300); ok {
		t.Fatal("deleted key still present")
	}
	if b.len() != len(want) {
		t.Fatalf("len after del = %d, want %d", b.len(), len(want))
	}
	for k, v := range want {
		if got, ok := b.get(k); !ok || !sameValue(t, got, v) {
			t.Fatalf("key %d disturbed by the delete: got %#v ok=%v, want %#v", k, got, ok, v)
		}
	}

	// forEach must visit every surviving record exactly once.
	seen := map[PropertyKeyID]int{}
	b.forEach(func(k PropertyKeyID, v PropertyValue) {
		seen[k]++
		if !sameValue(t, v, want[k]) {
			t.Fatalf("forEach yielded %#v for key %d, want %#v", v, k, want[k])
		}
	})
	if len(seen) != len(want) {
		t.Fatalf("forEach visited %d distinct keys, want %d", len(seen), len(want))
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("forEach visited key %d %d times, want once", k, n)
		}
	}

	// Deleting the last record must report empty so the caller drops the entry.
	for k := range want {
		b.del(k)
	}
	if b.len() != 0 {
		t.Fatalf("len after deleting everything = %d, want 0", b.len())
	}
}

// TestPropBagEncoding_AliasedStringSurvivesLaterMutation is the test for the
// immutability invariant that licenses unsafe.String in the decoder. A string
// handed out by get must not change when the bag is mutated afterwards; if a
// mutation ever rewrote a published buffer in place, this is what would catch
// it.
func TestPropBagEncoding_AliasedStringSurvivesLaterMutation(t *testing.T) {
	var b propBag
	b.set(1, StringValue("original-value"))
	held, _ := b.get(1)
	heldStr, _ := held.String()

	// The SAME-WIDTH overwrite must come FIRST, with nothing between it and
	// the get. It is the only case in which an implementation could plausibly
	// patch the record in place instead of rebuilding, and mutation testing
	// showed the assertion is worthless if anything else runs first: any other
	// mutation reallocates the buffer, after which the alias points at an old
	// array that an in-place patch would never touch, and the test passes over
	// a broken implementation.
	b.set(1, StringValue("REPLACED-VALUE")) // exactly len("original-value")
	if heldStr != "original-value" {
		t.Fatalf("a same-width overwrite mutated a string returned by get to %q; a published buffer must never be rewritten in place", heldStr)
	}

	// The remaining mutation shapes, which all rebuild the buffer.
	b.set(2, Int64Value(1234))
	b.set(1, StringValue("REPLACED"))
	b.set(3, StringValue(strings.Repeat("q", 500)))
	b.del(2)
	for i := 10; i < 10+smallBagMax; i++ {
		b.set(PropertyKeyID(i), Int64Value(int64(i)))
	}

	if heldStr != "original-value" {
		t.Fatalf("a string returned by get mutated to %q after later writes; the buffer must never be rewritten in place", heldStr)
	}
}

// TestPropBagEncoding_RapidRoundTrip generates arbitrary bags of streamable
// values and checks the bag agrees with a plain Go map at every step. The
// table above covers the edges it knows about; this covers the ones it does not.
func TestPropBagEncoding_RapidRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var b propBag
		model := map[PropertyKeyID]PropertyValue{}

		genVal := func(rt *rapid.T) PropertyValue {
			switch rapid.IntRange(0, 3).Draw(rt, "kind") {
			case 0:
				return Int64Value(rapid.Int64().Draw(rt, "int"))
			case 1:
				return Float64Value(rapid.Float64().Draw(rt, "float"))
			case 2:
				return BoolValue(rapid.Bool().Draw(rt, "bool"))
			default:
				return StringValue(rapid.String().Draw(rt, "str"))
			}
		}

		for range rapid.IntRange(1, 40).Draw(rt, "ops") {
			key := PropertyKeyID(rapid.Uint32Range(0, 70000).Draw(rt, "key"))
			if rapid.Bool().Draw(rt, "isDel") && len(model) > 0 {
				b.del(key)
				delete(model, key)
			} else {
				v := genVal(rt)
				b.set(key, v)
				model[key] = v
			}

			if b.len() != len(model) {
				rt.Fatalf("len = %d, model has %d", b.len(), len(model))
			}
			for k, want := range model {
				got, ok := b.get(k)
				if !ok {
					rt.Fatalf("key %d absent from the bag but present in the model", k)
				}
				if !sameValue(t, got, want) {
					rt.Fatalf("key %d: bag has %#v, model has %#v", k, got, want)
				}
			}
		}
	})
}

// TestPropBagEncoding_UsesNarrowestWidths pins the COMPACTNESS the byte stream
// exists for, which no round-trip test can see: a symmetric change to both
// encoder and decoder round-trips perfectly while doubling the footprint, and a
// mutation test confirmed exactly that. These assertions are on the encoded
// SIZE, so they fail when the encoding stops being narrow.
func TestPropBagEncoding_UsesNarrowestWidths(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  PropertyKeyID
		val  PropertyValue
		want int // metadata + id + payload
	}{
		{"small key, small positive int", 1, Int64Value(1), 1 + 1 + 1},
		{"small key, small NEGATIVE int", 1, Int64Value(-1), 1 + 1 + 1},
		{"small key, -64", 1, Int64Value(-64), 1 + 1 + 1},
		{"small key, MinInt64", 1, Int64Value(math.MinInt64), 1 + 1 + 8},
		{"wide key, small int", 70000, Int64Value(1), 1 + 4 + 1},
		{"bool costs no payload", 1, BoolValue(true), 1 + 1},
		{"9-char string", 1, StringValue("p00000001"), 1 + 1 + 1 + 9},
		{"300-char string needs a 2-byte length", 1, StringValue(strings.Repeat("a", 300)), 1 + 1 + 2 + 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b propBag
			b.set(tc.key, tc.val)
			if got := len(b.buf); got != tc.want {
				t.Fatalf("encoded to %d bytes, want %d — the encoding is no longer using the narrowest width", got, tc.want)
			}
		})
	}

	// The whole three-property shape the audit measured, which Memgraph
	// encodes in 26 bytes. Matching that number is the point of the exercise.
	var b propBag
	b.set(1, StringValue("p00000001"))
	b.set(2, StringValue("person-1"))
	b.set(3, Int64Value(25))
	if got, want := len(b.buf), 26; got != want {
		t.Fatalf("the audit's three-property node encodes to %d bytes, want %d", got, want)
	}
}
