package lpg

import (
	"math"
	"strings"
	"testing"
)

// TestBagStringAtAgreesWithDecodeAt pins [propBag.getString] to [propBag.get]
// on both bag tiers, for every kind and the string length-width boundaries, and
// checks that a string read allocates nothing (rmp #3057).
func TestBagStringAtAgreesWithDecodeAt(t *testing.T) {
	values := []PropertyValue{
		BoolValue(true), Int64Value(-1), Int64Value(math.MaxInt64), Float64Value(1.5),
		StringValue(""), StringValue("a"), StringValue(strings.Repeat("x", 255)),
		StringValue(strings.Repeat("y", 256)), StringValue(strings.Repeat("z", 70000)),
	}
	keys := []PropertyKeyID{0, 255, 256, 65536, math.MaxUint32}
	check := func(t *testing.T, b *propBag, k PropertyKeyID) {
		t.Helper()
		want, wantOK := b.get(k)
		s, isString, ok := b.getString(k)
		if ok != wantOK || isString != (wantOK && want.Kind() == PropString) {
			t.Fatalf("key %d: getString = (isString %v, ok %v), get = (%v, %v)", k, isString, ok, want.Kind(), wantOK)
		}
		if isString {
			if ws, _ := want.String(); s != ws {
				t.Fatalf("key %d: getString = %q, get = %q", k, s, ws)
			}
		}
	}
	for _, v := range values {
		for _, k := range keys {
			var b propBag
			b.set(k+1, Int64Value(7)) // the record under test is never at offset 0
			b.set(k, v)
			if b.buf == nil {
				t.Fatal("expected the stream tier")
			}
			check(t, &b, k)
			check(t, &b, k+2) // absent
			b.promote(0)
			if b.m == nil {
				t.Fatal("expected the map tier")
			}
			check(t, &b, k)
			check(t, &b, k+2)
		}
	}
	var b propBag
	b.set(3, StringValue("payload"))
	if n := testing.AllocsPerRun(100, func() { _, _, _ = b.getString(3) }); n != 0 {
		t.Errorf("getString allocated %.0f times per string read, want 0", n)
	}
}
