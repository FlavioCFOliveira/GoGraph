package packstream_test

// fidelity_test.go — regression gate for #1799 (sprint 251). FuzzDecodeValue
// only checks no-panic + re-decodability; it does NOT assert that a value
// survives encode->decode unchanged. These tests close that gap: a NaN-aware
// value comparator drives both a fuzz target (decode->encode->decode must be
// value-stable) and a table of width/length-tag boundary values
// (value->encode->decode must be identical).

import (
	"bytes"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// valuesEqual is a NaN-aware deep equality for packstream.Value. reflect.DeepEqual
// is unusable here because NaN != NaN; floats are compared by bit pattern so
// every distinct NaN/zero encoding round-trips exactly.
func valuesEqual(a, b packstream.Value) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && math.Float64bits(av) == math.Float64bits(bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case []packstream.Value:
		bv, ok := b.([]packstream.Value)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]packstream.Value:
		bv, ok := b.(map[string]packstream.Value)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, present := bv[k]
			if !present || !valuesEqual(va, vb) {
				return false
			}
		}
		return true
	case packstream.Struct:
		bv, ok := b.(packstream.Struct)
		if !ok || av.Tag != bv.Tag || len(av.Fields) != len(bv.Fields) {
			return false
		}
		for i := range av.Fields {
			if !valuesEqual(av.Fields[i], bv.Fields[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func fidelityRoundTrip(t *testing.T, v packstream.Value) packstream.Value {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(v); err != nil {
		t.Fatalf("WriteValue(%#v): %v", v, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue(%#v): %v", v, err)
	}
	return got
}

// TestDecodeFidelity_Boundaries pins encode->decode value-fidelity at every
// int width tag, string/bytes length tag, and float special boundary.
func TestDecodeFidelity_Boundaries(t *testing.T) {
	mk := func(n int) string { return string(bytes.Repeat([]byte{'a'}, n)) }
	cases := []packstream.Value{
		// int width-tag selection boundaries (decoder normalises to int64).
		int64(0), int64(-16), int64(-17), int64(127), int64(128),
		int64(math.MinInt8), int64(math.MinInt8 - 1),
		int64(math.MaxInt8), int64(math.MaxInt8 + 1),
		int64(math.MinInt16), int64(math.MinInt16 - 1),
		int64(math.MaxInt16), int64(math.MaxInt16 + 1),
		int64(math.MinInt32), int64(math.MinInt32 - 1),
		int64(math.MaxInt32), int64(math.MaxInt32 + 1),
		int64(math.MinInt64), int64(math.MaxInt64),
		// string length-tag boundaries.
		"", mk(15), mk(16), mk(255), mk(256), mk(65535), mk(65536),
		// bytes length-tag boundaries.
		[]byte{}, bytes.Repeat([]byte{0xAB}, 255), bytes.Repeat([]byte{0xCD}, 256),
		bytes.Repeat([]byte{0xEF}, 65536),
		// float specials / extremes (bit-exact).
		0.0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.NaN(),
		math.MaxFloat64, math.SmallestNonzeroFloat64, 3.141592653589793,
		// nested + composite.
		[]packstream.Value{int64(1), "x", []packstream.Value{true, nil}},
		map[string]packstream.Value{"k": int64(65536), "f": math.Inf(1)},
		packstream.Struct{Tag: 0x70, Fields: []packstream.Value{int64(-17), "s"}},
	}
	for _, v := range cases {
		got := fidelityRoundTrip(t, v)
		if !valuesEqual(v, got) {
			t.Errorf("fidelity: round-trip of %#v gave %#v", v, got)
		}
	}
}

// FuzzDecodeFidelity decodes arbitrary bytes; on success it re-encodes and
// re-decodes and asserts the two decoded VALUES are equal (not merely that the
// second decode succeeded). This catches encode/decode fidelity drift that
// FuzzDecodeValue cannot.
func FuzzDecodeFidelity(f *testing.F) {
	for _, v := range []packstream.Value{
		nil, true, int64(-17), int64(65536), 3.14, math.NaN(),
		"", "hello", []byte{0x01, 0x02},
		[]packstream.Value{int64(1), int64(2)},
		map[string]packstream.Value{"k": int64(42)},
		packstream.Struct{Tag: 0x01, Fields: []packstream.Value{int64(1)}},
	} {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if enc.WriteValue(v) == nil && enc.Flush() == nil {
			f.Add(buf.Bytes())
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		dec := packstream.NewDecoder(bytes.NewReader(data))
		v1, err := dec.ReadValue()
		if err != nil {
			return // malformed input is acceptable
		}
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if err := enc.WriteValue(v1); err != nil {
			t.Fatalf("WriteValue after successful ReadValue: %v", err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		v2, err := packstream.NewDecoder(&buf).ReadValue()
		if err != nil {
			t.Fatalf("second ReadValue failed: %v", err)
		}
		if !valuesEqual(v1, v2) {
			t.Fatalf("fidelity drift: %#v != %#v", v1, v2)
		}
	})
}
