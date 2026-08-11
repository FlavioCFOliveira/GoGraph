package lpg

import (
	"math"
	"strings"
	"testing"
)

// bagKeyAt exists to skip the value decode when a scan only needs the key, so
// its ONLY contract is that it agrees with bagDecodeAt on the key and on where
// the next record starts. If the two ever disagree about `next`, every scan
// silently walks into the middle of a record and reads garbage — the failure
// would not be a crash but wrong property values, which is why this is pinned
// exhaustively rather than by sampling.
func TestBagKeyAtAgreesWithDecodeAt(t *testing.T) {
	// Values chosen to exercise every kind and every payload width the encoder
	// selects between: a bool (payload rides in the metadata byte), integers at
	// each zig-zag width boundary and both signs, a float, and strings at the
	// length-field width boundaries including empty.
	values := []PropertyValue{
		BoolValue(true),
		BoolValue(false),
		Int64Value(0),
		Int64Value(1),
		Int64Value(-1),
		Int64Value(127),
		Int64Value(-128),
		Int64Value(math.MaxInt16),
		Int64Value(math.MinInt16),
		Int64Value(math.MaxInt32),
		Int64Value(math.MinInt32),
		Int64Value(math.MaxInt64),
		Int64Value(math.MinInt64),
		Float64Value(0),
		Float64Value(-0),
		Float64Value(1.5),
		Float64Value(math.Inf(1)),
		Float64Value(math.NaN()),
		StringValue(""),
		StringValue("a"),
		StringValue(strings.Repeat("x", 255)),
		StringValue(strings.Repeat("y", 256)),
		StringValue(strings.Repeat("z", 70000)),
	}

	// Key ids chosen across the id-width boundaries the metadata byte encodes.
	keys := []PropertyKeyID{0, 1, 255, 256, 65535, 65536, math.MaxUint32}

	// Each case gets its OWN bag, because the stream tier holds only
	// smallBagMax records before promoting to a map and discarding the buffer;
	// one bag holding every case would test the map tier instead.
	for i, v := range values {
		for _, k := range keys {
			var b propBag
			// A leading record of a different kind, so the case under test is
			// never at offset 0 and a wrong `next` from the first record is
			// caught as a misread of the second.
			b.set(k+1, Int64Value(int64(i)))
			b.set(k, v)
			if b.buf == nil {
				t.Fatalf("value %d key %d: bag promoted unexpectedly", i, k)
			}

			records := 0
			for off := 0; off < len(b.buf); {
				fullRec := bagDecodeAt(b.buf, off)
				gotKey, gotNext := bagKeyAt(b.buf, off)
				if gotKey != fullRec.key {
					t.Fatalf("value %d key %d, record %d at offset %d: bagKeyAt key=%d, bagDecodeAt key=%d",
						i, k, records, off, gotKey, fullRec.key)
				}
				if gotNext != fullRec.next {
					t.Fatalf("value %d key %d, record %d at offset %d: bagKeyAt next=%d, bagDecodeAt next=%d — "+
						"a scan would resume inside a record and read garbage",
						i, k, records, off, gotNext, fullRec.next)
				}
				records++
				off = gotNext
			}
			if records != 2 {
				t.Fatalf("value %d key %d: walked %d records, want 2", i, k, records)
			}

			// The value must still read back through the key-only scan, which
			// proves get lands on the right record rather than merely agreeing
			// with a decoder that is itself wrong.
			got, ok := b.get(k)
			if !ok {
				t.Fatalf("value %d key %d: not found after the key-only scan", i, k)
			}
			if got.Kind() != v.Kind() {
				t.Fatalf("value %d key %d: kind %v, want %v", i, k, got.Kind(), v.Kind())
			}
		}
	}
}

// TestBagKeyAtSkipsToTheRightRecordOnDelete pins the other scan that now uses
// the key-only decoder and then splices the buffer by the offsets it returned.
// An off-by-one in `next` here corrupts the stream rather than merely misreading
// it, so the surviving records are checked explicitly.
func TestBagKeyAtSkipsToTheRightRecordOnDelete(t *testing.T) {
	var b propBag
	b.set(1, StringValue("one"))
	b.set(2, Int64Value(2))
	b.set(3, StringValue("three"))
	b.set(4, Float64Value(4.5))

	if b.m != nil {
		t.Skip("bag promoted to the map tier; this test is about the stream")
	}

	b.del(2)

	if _, ok := b.get(2); ok {
		t.Error("deleted key 2 is still present")
	}
	for _, tc := range []struct {
		key  PropertyKeyID
		kind PropertyKind
	}{{1, PropString}, {3, PropString}, {4, PropFloat64}} {
		got, ok := b.get(tc.key)
		if !ok {
			t.Errorf("key %d lost after deleting a neighbour: the splice used a wrong offset", tc.key)
			continue
		}
		if got.Kind() != tc.kind {
			t.Errorf("key %d: kind %v, want %v — the stream is misaligned", tc.key, got.Kind(), tc.kind)
		}
	}
	if n := b.count(); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}
