package packstream_test

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"gograph/bolt/packstream"
)

// PackStream marker bytes reproduced locally so the tests can hand-craft
// adversarial frames without exporting the unexported constants. Values are
// from the PackStream v2 / Bolt v5 specification.
const (
	markerBytes32 = 0xCE // Bytes with a uint32 length prefix.
	markerStr32   = 0xD2 // String with a uint32 length prefix.
	markerList32  = 0xD6 // List with a uint32 count prefix.
	markerMap32   = 0xDA // Map with a uint32 count prefix.
)

// hugeLen32 is a uint32 length prefix of 0xFFFFFFFF (~4.29e9). As a Bytes or
// String length it requests a ~4.29 GB payload; as a List count it requests
// ~64 GB of 16-byte interface slots. Either would OOM the process if the
// decoder allocated before validating against the bytes actually available.
var hugeLen32 = []byte{0xFF, 0xFF, 0xFF, 0xFF}

// assertBoundedAlloc runs fn and fails if it allocated more than maxBytes of
// heap. It is the load-bearing assertion for the security fix: a decode that
// rejects an oversized length prefix must NOT first commit the multi-gigabyte
// make() the prefix requested. TotalAlloc counts cumulative bytes allocated,
// so the attempted make() would show up as a multi-GB delta even though the
// allocation is immediately abandoned. A few KiB of legitimate bookkeeping is
// expected, so the cap is generous (1 MiB) while still being many orders of
// magnitude below the gigabytes the attack would commit.
func assertBoundedAlloc(t *testing.T, maxBytes uint64, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta > maxBytes {
		t.Fatalf("allocated %d bytes, want <= %d (oversized length prefix was not rejected before make)", delta, maxBytes)
	}
}

// TestReadBytesLengthExceedsInput is the core regression test for security fix
// H2 on the Bytes arm: a 5-byte frame 0xCE 0xFF 0xFF 0xFF 0xFF claims a
// ~4.29 GB payload. Before the fix the decoder ran make([]byte, n) — a
// multi-GB allocation — before the inevitable short read failed. The fix
// rejects the prefix with ErrLengthExceedsInput before allocating.
func TestReadBytesLengthExceedsInput(t *testing.T) {
	frame := append([]byte{markerBytes32}, hugeLen32...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadValue error = %v, want ErrLengthExceedsInput (value %v)", err, v)
		}
	})
}

// TestReadStringLengthExceedsInput is the same attack on the String arm:
// 0xD2 0xFF 0xFF 0xFF 0xFF requests a ~4.29 GB string buffer.
func TestReadStringLengthExceedsInput(t *testing.T) {
	frame := append([]byte{markerStr32}, hugeLen32...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadValue error = %v, want ErrLengthExceedsInput (value %v)", err, v)
		}
	})
}

// TestReadListLengthExceedsInput exercises the List arm: a 5-byte List32
// header 0xD6 0xFF 0xFF 0xFF 0xFF claims ~4.29e9 elements, which
// make([]Value, n) would size at ~64 GB (16 bytes per interface slot).
func TestReadListLengthExceedsInput(t *testing.T) {
	frame := append([]byte{markerList32}, hugeLen32...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadValue error = %v, want ErrLengthExceedsInput (value %v)", err, v)
		}
	})
}

// TestReadMapLengthExceedsInput exercises the Map arm: 0xDA 0xFF 0xFF 0xFF 0xFF
// claims ~4.29e9 entries, which make(map[string]Value, n) would pre-size.
func TestReadMapLengthExceedsInput(t *testing.T) {
	frame := append([]byte{markerMap32}, hugeLen32...)

	assertBoundedAlloc(t, 1<<20, func() {
		dec := packstream.NewDecoder(bytes.NewReader(frame))
		v, err := dec.ReadValue()
		if !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadValue error = %v, want ErrLengthExceedsInput (value %v)", err, v)
		}
	})
}

// TestLengthBoundFastReject confirms the rejection is immediate (no
// allocation, no large read) for each oversized arm, using the smaller
// per-call helpers in addition to the ReadValue dispatch path.
func TestLengthBoundFastReject(t *testing.T) {
	t.Run("Bytes", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(append([]byte{markerBytes32}, hugeLen32...)))
		if _, err := dec.ReadBytes(); !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadBytes error = %v, want ErrLengthExceedsInput", err)
		}
	})
	t.Run("String", func(t *testing.T) {
		dec := packstream.NewDecoder(bytes.NewReader(append([]byte{markerStr32}, hugeLen32...)))
		if _, err := dec.ReadString(); !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadString error = %v, want ErrLengthExceedsInput", err)
		}
	})
}

// TestLengthBoundValidRoundTrip is the positive regression: valid Bytes,
// String, List, Map, and Struct values within the buffer still decode
// correctly. The byte budget must never reject well-formed input.
func TestLengthBoundValidRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    packstream.Value
	}{
		{"bytes", []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		{"bytes_empty", []byte{}},
		{"string", "hello world"},
		{"string_empty", ""},
		{"string_long", strings.Repeat("x", 4096)},
		{"list", []packstream.Value{int64(1), int64(2), int64(3)}},
		{"list_nested", []packstream.Value{[]packstream.Value{"a", "b"}, int64(7)}},
		{"map", map[string]packstream.Value{"a": int64(1), "b": "two"}},
		{"struct", packstream.Struct{Tag: 0x4E, Fields: []packstream.Value{int64(1), "node"}}},
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

			dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
			got, err := dec.ReadValue()
			if err != nil {
				t.Fatalf("ReadValue rejected valid %s: %v", tc.name, err)
			}
			assertValueEqual(t, got, tc.v)
		})
	}
}

// TestLengthBoundMultipleTopLevelValues confirms the budget tracks consumption
// across several sequential values in one message: a string followed by a list
// in a single buffer both decode, proving remaining is decremented (not reset)
// as bytes are consumed and that the second value's prefix is validated
// against the bytes that genuinely remain.
func TestLengthBoundMultipleTopLevelValues(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	for _, v := range []packstream.Value{"first", []packstream.Value{int64(1), int64(2)}, int64(42)} {
		if err := enc.WriteValue(v); err != nil {
			t.Fatalf("WriteValue: %v", err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))

	s, err := dec.ReadValue()
	if err != nil || s != "first" {
		t.Fatalf("first value = %v, %v; want \"first\", nil", s, err)
	}
	lst, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("second value: %v", err)
	}
	assertValueEqual(t, lst, []packstream.Value{int64(1), int64(2)})
	n, err := dec.ReadValue()
	if err != nil || n != int64(42) {
		t.Fatalf("third value = %v, %v; want 42, nil", n, err)
	}
}

// TestLengthBoundStreamingFallback confirms the fallback path for a reader
// whose length is unknown (here a custom io.Reader, not a *bytes.Reader).
// The decoder cannot know the exact remaining bytes, so it caps allocations
// at the default 16 MiB message ceiling: a uint32 length prefix far above
// that ceiling is still rejected with ErrLengthExceedsInput rather than
// allocating ~4.29 GB.
func TestLengthBoundStreamingFallback(t *testing.T) {
	frame := append([]byte{markerBytes32}, hugeLen32...)

	assertBoundedAlloc(t, 1<<20, func() {
		// opaqueReader hides the underlying *bytes.Reader so sourceLen falls
		// through to the unknown-length branch.
		dec := packstream.NewDecoder(opaqueReader{bytes.NewReader(frame)})
		if _, err := dec.ReadValue(); !errors.Is(err, packstream.ErrLengthExceedsInput) {
			t.Fatalf("ReadValue error = %v, want ErrLengthExceedsInput on streaming fallback", err)
		}
	})
}

// opaqueReader wraps an io.Reader so the decoder cannot type-assert it to a
// length-bearing concrete reader, forcing the unknown-length fallback budget.
type opaqueReader struct{ inner io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.inner.Read(p) }

// assertValueEqual compares two decoded PackStream values for structural
// equality, recursing into lists, maps, and structs.
func assertValueEqual(t *testing.T, got, want packstream.Value) {
	t.Helper()
	switch w := want.(type) {
	case []byte:
		g, ok := got.([]byte)
		if !ok || !bytes.Equal(g, w) {
			t.Fatalf("bytes mismatch: got %v (%T), want %v", got, got, w)
		}
	case []packstream.Value:
		g, ok := got.([]packstream.Value)
		if !ok || len(g) != len(w) {
			t.Fatalf("list mismatch: got %v (%T), want %v", got, got, w)
		}
		for i := range w {
			assertValueEqual(t, g[i], w[i])
		}
	case map[string]packstream.Value:
		g, ok := got.(map[string]packstream.Value)
		if !ok || len(g) != len(w) {
			t.Fatalf("map mismatch: got %v (%T), want %v", got, got, w)
		}
		for k, wv := range w {
			assertValueEqual(t, g[k], wv)
		}
	case packstream.Struct:
		g, ok := got.(packstream.Struct)
		if !ok || g.Tag != w.Tag || len(g.Fields) != len(w.Fields) {
			t.Fatalf("struct mismatch: got %v (%T), want %v", got, got, w)
		}
		for i := range w.Fields {
			assertValueEqual(t, g.Fields[i], w.Fields[i])
		}
	default:
		if got != want {
			t.Fatalf("scalar mismatch: got %v (%T), want %v (%T)", got, got, want, want)
		}
	}
}
