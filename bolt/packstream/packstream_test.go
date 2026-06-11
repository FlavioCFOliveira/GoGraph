package packstream_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// roundTrip encodes v and decodes it back, returning the decoded Value.
func roundTrip(t *testing.T, v packstream.Value) packstream.Value {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(v); err != nil {
		t.Fatalf("WriteValue(%v): %v", v, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	return got
}

func TestNullRoundTrip(t *testing.T) {
	got := roundTrip(t, nil)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestBoolRoundTrip(t *testing.T) {
	for _, want := range []bool{true, false} {
		got := roundTrip(t, want)
		if got != want {
			t.Errorf("bool %v: got %v", want, got)
		}
	}
}

func TestIntRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    int64
	}{
		{"zero", 0},
		{"one", 1},
		{"minus_one", -1},
		{"tinyInt_low", -16},
		{"tinyInt_high", 127},
		{"tinyInt_just_below", -17},
		{"tinyInt_just_above", 128},
		{"int8_min", math.MinInt8},
		{"int8_max", math.MaxInt8},
		{"int16_min", math.MinInt16},
		{"int16_max", math.MaxInt16},
		{"int32_min", math.MinInt32},
		{"int32_max", math.MaxInt32},
		{"int64_min", math.MinInt64},
		{"int64_max", math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			if got != tc.v {
				t.Errorf("want %d, got %v", tc.v, got)
			}
		})
	}
}

func TestFloatRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    float64
	}{
		{"zero", 0},
		{"pi", math.Pi},
		{"neg_inf", math.Inf(-1)},
		{"pos_inf", math.Inf(1)},
		{"nan", math.NaN()},
		{"max", math.MaxFloat64},
		{"smallest_nonzero", math.SmallestNonzeroFloat64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			gf, ok := got.(float64)
			if !ok {
				t.Fatalf("expected float64, got %T", got)
			}
			if math.IsNaN(tc.v) {
				if !math.IsNaN(gf) {
					t.Errorf("want NaN, got %v", gf)
				}
			} else if gf != tc.v {
				t.Errorf("want %v, got %v", tc.v, gf)
			}
		})
	}
}

func TestBytesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    []byte
	}{
		{"empty", []byte{}},
		{"one_byte", []byte{0xFF}},
		{"255_bytes", bytes.Repeat([]byte{0xAB}, 255)},
		{"256_bytes", bytes.Repeat([]byte{0xCD}, 256)},
		{"65535_bytes", bytes.Repeat([]byte{0xEF}, 65535)},
		{"65536_bytes", bytes.Repeat([]byte{0x01}, 65536)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			gb, ok := got.([]byte)
			if !ok {
				t.Fatalf("expected []byte, got %T", got)
			}
			if !bytes.Equal(gb, tc.v) {
				t.Errorf("bytes mismatch (len want=%d got=%d)", len(tc.v), len(gb))
			}
		})
	}
}

func TestStringRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    string
	}{
		{"empty", ""},
		{"tiny_max", strings.Repeat("x", 15)},
		{"tiny_max_plus_1", strings.Repeat("x", 16)},
		{"ascii_255", strings.Repeat("a", 255)},
		{"ascii_256", strings.Repeat("a", 256)},
		{"ascii_65535", strings.Repeat("b", 65535)},
		{"utf8", "héllo wörld"},
		{"emoji", "hello 🌍"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			gs, ok := got.(string)
			if !ok {
				t.Fatalf("expected string, got %T", got)
			}
			if gs != tc.v {
				t.Errorf("want %q, got %q", tc.v, gs)
			}
		})
	}
}

func TestListRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    []packstream.Value
	}{
		{"empty", []packstream.Value{}},
		{"tiny_max", makeIntList(15)},
		{"tiny_max_plus_1", makeIntList(16)},
		{"256_elements", makeIntList(256)},
		{"mixed", []packstream.Value{nil, true, int64(42), "hello", []byte{0xBE, 0xEF}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			gl, ok := got.([]packstream.Value)
			if !ok {
				t.Fatalf("expected []Value, got %T", got)
			}
			if len(gl) != len(tc.v) {
				t.Fatalf("length mismatch: want %d, got %d", len(tc.v), len(gl))
			}
		})
	}
}

func makeIntList(n int) []packstream.Value {
	out := make([]packstream.Value, n)
	for i := range out {
		out[i] = int64(i)
	}
	return out
}

func TestMapRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    map[string]packstream.Value
	}{
		{"empty", map[string]packstream.Value{}},
		{"tiny_max", makeStringMap(15)},
		{"tiny_max_plus_1", makeStringMap(16)},
		{"256_pairs", makeStringMap(256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			gm, ok := got.(map[string]packstream.Value)
			if !ok {
				t.Fatalf("expected map[string]Value, got %T", got)
			}
			if len(gm) != len(tc.v) {
				t.Fatalf("length mismatch: want %d, got %d", len(tc.v), len(gm))
			}
			for k, wantVal := range tc.v {
				gotVal, exists := gm[k]
				if !exists {
					t.Errorf("key %q missing from decoded map", k)
					continue
				}
				if gotVal != wantVal {
					t.Errorf("key %q: want %v, got %v", k, wantVal, gotVal)
				}
			}
		})
	}
}

func makeStringMap(n int) map[string]packstream.Value {
	m := make(map[string]packstream.Value, n)
	for i := range n {
		k := string(rune('a'+i%26)) + string(rune('0'+i%10)) // unique-ish keys
		m[k+string(rune(i))] = int64(i)
	}
	return m
}

func TestStructRoundTrip(t *testing.T) {
	s := packstream.Struct{
		Tag:    0x01,
		Fields: []packstream.Value{int64(42), "hello", nil},
	}
	got := roundTrip(t, s)
	gs, ok := got.(packstream.Struct)
	if !ok {
		t.Fatalf("expected Struct, got %T", got)
	}
	if gs.Tag != s.Tag {
		t.Errorf("tag: want 0x%02X, got 0x%02X", s.Tag, gs.Tag)
	}
	if len(gs.Fields) != len(s.Fields) {
		t.Fatalf("fields len: want %d, got %d", len(s.Fields), len(gs.Fields))
	}
}

func TestStructMax15Fields(t *testing.T) {
	fields := make([]packstream.Value, 15)
	for i := range fields {
		fields[i] = int64(i)
	}
	s := packstream.Struct{Tag: 0x42, Fields: fields}
	got := roundTrip(t, s)
	gs, ok := got.(packstream.Struct)
	if !ok {
		t.Fatalf("expected Struct, got %T", got)
	}
	if len(gs.Fields) != 15 {
		t.Errorf("want 15 fields, got %d", len(gs.Fields))
	}
}

func TestWriteStructHeaderOutOfRange(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteStructHeader(0x01, 16); err == nil {
		t.Fatal("expected error for struct with 16 fields, got nil")
	}
}

func TestPeekType(t *testing.T) {
	cases := []struct {
		name string
		v    packstream.Value
		want packstream.Type
	}{
		{"null", nil, packstream.TypeNull},
		{"bool_true", true, packstream.TypeBool},
		{"bool_false", false, packstream.TypeBool},
		{"int", int64(1), packstream.TypeInt},
		{"float", 1.5, packstream.TypeFloat},
		{"bytes", []byte{0x01}, packstream.TypeBytes},
		{"string", "hi", packstream.TypeString},
		{"list", []packstream.Value{}, packstream.TypeList},
		{"map", map[string]packstream.Value{}, packstream.TypeMap},
		{"struct", packstream.Struct{Tag: 0x01, Fields: nil}, packstream.TypeStruct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			if err := enc.WriteValue(tc.v); err != nil {
				t.Fatalf("WriteValue: %v", err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			dec := packstream.NewDecoder(&buf)
			got, err := dec.PeekType()
			if err != nil {
				t.Fatalf("PeekType: %v", err)
			}
			if got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestEncodePool(t *testing.T) {
	pool := packstream.NewEncodePool()
	var buf bytes.Buffer
	enc := pool.Get(&buf)
	if err := enc.WriteInt(int64(42)); err != nil {
		t.Fatalf("WriteInt: %v", err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	pool.Put(enc)

	dec := packstream.NewDecoder(&buf)
	v, err := dec.ReadInt()
	if err != nil {
		t.Fatalf("ReadInt: %v", err)
	}
	if v != 42 {
		t.Errorf("want 42, got %d", v)
	}
}

func TestDecodePool(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteString("hello"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	pool := packstream.NewDecodePool()
	r := bytes.NewReader(buf.Bytes())
	dec := pool.Get(r)
	got, err := dec.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	pool.Put(dec)
	if got != "hello" {
		t.Errorf("want hello, got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Wrong-marker error paths for all primitive decoders
// ─────────────────────────────────────────────────────────────────────────────

func TestReadNullWrongMarker(t *testing.T) {
	// 0xC3 = markerTrue, not markerNull (0xC0)
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC3}))
	if err := dec.ReadNull(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadBoolWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a bool marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadBool(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadFloatWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not markerFloat64 (0xC1)
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadFloat(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadIntWrongMarker(t *testing.T) {
	// 0x80 = TinyStr base — not in TinyInt range and not an Int8/16/32/64 marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0x80}))
	if _, err := dec.ReadInt(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadBytesWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a bytes marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadBytes(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadStringWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a string marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadString(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadListHeaderWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a list marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadListHeader(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadMapHeaderWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a map marker
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, err := dec.ReadMapHeader(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

func TestReadStructHeaderWrongMarker(t *testing.T) {
	// 0xC0 = markerNull, not a struct marker (0xB0..0xBF)
	dec := packstream.NewDecoder(bytes.NewReader([]byte{0xC0}))
	if _, _, err := dec.ReadStructHeader(); err == nil {
		t.Fatal("expected error for wrong marker")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Large-size variants: List32, Map32, Str32 (n > 65535)
// ─────────────────────────────────────────────────────────────────────────────

func TestWriteListHeaderNegative(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteListHeader(-1); err == nil {
		t.Fatal("expected error for negative list length")
	}
}

func TestWriteMapHeaderNegative(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteMapHeader(-1); err == nil {
		t.Fatal("expected error for negative map length")
	}
}

// TestListHeaderList32 exercises the List32 encoder/decoder (n > 65535).
// ReadListHeader validates the count against the bytes still available, so
// the n declared elements (one NULL byte each) are serialised after the
// 5-byte header to make the message structurally complete.
func TestListHeaderList32(t *testing.T) {
	const n = 65536
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteListHeader(n); err != nil {
		t.Fatalf("WriteListHeader(%d): %v", n, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	buf.Write(bytes.Repeat([]byte{0xC0}, n)) // n NULL elements
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadListHeader()
	if err != nil {
		t.Fatalf("ReadListHeader: %v", err)
	}
	if got != n {
		t.Errorf("want %d, got %d", n, got)
	}
}

// TestMapHeaderMap32 exercises the Map32 encoder/decoder (n > 65535).
// ReadMapHeader validates the count against the bytes still available (two
// per entry), so the n declared entries (empty-string key + NULL value) are
// serialised after the 5-byte header to make the message structurally
// complete.
func TestMapHeaderMap32(t *testing.T) {
	const n = 65536
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteMapHeader(n); err != nil {
		t.Fatalf("WriteMapHeader(%d): %v", n, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	buf.Write(bytes.Repeat([]byte{0x80, 0xC0}, n)) // n entries: "" -> NULL
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadMapHeader()
	if err != nil {
		t.Fatalf("ReadMapHeader: %v", err)
	}
	if got != n {
		t.Errorf("want %d, got %d", n, got)
	}
}

// TestStringStr32 round-trips a string that requires the Str32 marker (n > 65535).
func TestStringStr32(t *testing.T) {
	const n = 65536
	v := strings.Repeat("z", n)
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteString(v); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if len(got) != n {
		t.Errorf("length want %d, got %d", n, len(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WriteValue: int, int32 native types and unsupported type error
// ─────────────────────────────────────────────────────────────────────────────

func TestWriteValueIntTypes(t *testing.T) {
	cases := []struct {
		name string
		v    packstream.Value
		want int64
	}{
		{"int", int(42), 42},
		{"int32", int32(100), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			if err := enc.WriteValue(tc.v); err != nil {
				t.Fatalf("WriteValue: %v", err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatal(err)
			}
			dec := packstream.NewDecoder(&buf)
			got, err := dec.ReadInt()
			if err != nil {
				t.Fatalf("ReadInt: %v", err)
			}
			if got != tc.want {
				t.Errorf("want %d, got %d", tc.want, got)
			}
		})
	}
}

func TestWriteValueUnsupportedType(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	// complex128 is not a PackStream value type
	if err := enc.WriteValue(complex(1.0, 2.0)); err == nil {
		t.Fatal("expected error for unsupported type, got nil")
	}
}

// TestLowLevelPrimitives tests the low-level Encoder/Decoder methods directly
// (not via WriteValue/ReadValue) to ensure they work independently.
func TestLowLevelPrimitives(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)

	if err := enc.WriteNull(); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteBool(true); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteInt(-16); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteFloat(3.14); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteBytes([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteListHeader(2); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteInt(1); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteInt(2); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteMapHeader(1); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteString("k"); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteInt(99); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteStructHeader(0x42, 1); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteNull(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}

	dec := packstream.NewDecoder(&buf)
	if err := dec.ReadNull(); err != nil {
		t.Fatal(err)
	}
	b, err := dec.ReadBool()
	if err != nil || !b {
		t.Fatalf("ReadBool: got %v, %v", b, err)
	}
	i, err := dec.ReadInt()
	if err != nil || i != -16 {
		t.Fatalf("ReadInt: got %d, %v", i, err)
	}
	f, err := dec.ReadFloat()
	if err != nil || math.Abs(f-3.14) > 1e-10 {
		t.Fatalf("ReadFloat: got %v, %v", f, err)
	}
	bs, err := dec.ReadBytes()
	if err != nil || !bytes.Equal(bs, []byte{1, 2, 3}) {
		t.Fatalf("ReadBytes: got %v, %v", bs, err)
	}
	s, err := dec.ReadString()
	if err != nil || s != "abc" {
		t.Fatalf("ReadString: got %q, %v", s, err)
	}
	n, err := dec.ReadListHeader()
	if err != nil || n != 2 {
		t.Fatalf("ReadListHeader: got %d, %v", n, err)
	}
	for range 2 {
		if _, err := dec.ReadInt(); err != nil {
			t.Fatal(err)
		}
	}
	mn, err := dec.ReadMapHeader()
	if err != nil || mn != 1 {
		t.Fatalf("ReadMapHeader: got %d, %v", mn, err)
	}
	sk, err := dec.ReadString()
	if err != nil || sk != "k" {
		t.Fatalf("ReadString (map key): got %q, %v", sk, err)
	}
	if _, err := dec.ReadInt(); err != nil {
		t.Fatal(err)
	}
	tag, fn, err := dec.ReadStructHeader()
	if err != nil || tag != 0x42 || fn != 1 {
		t.Fatalf("ReadStructHeader: tag=0x%02X n=%d err=%v", tag, fn, err)
	}
	if err := dec.ReadNull(); err != nil {
		t.Fatal(err)
	}
}
