package packstream_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// unknownBytes contains bytes that are not assigned to any PackStream type in
// markerTypeTable. They fall through to TypeNull (the zero value) because the
// lookup table defaults to zero for unrecognized bytes. When ReadValue sees
// TypeNull it calls ReadNull, which rejects any marker that is not 0xC0 with
// a descriptive error. There is currently no typed sentinel for this
// condition; the error is an untyped string produced by fmt.Errorf.
//
// NOTE: A typed sentinel (ErrUnknownTag or similar) is not yet implemented.
// If that is added in the future, the assertions below should be updated to
// use errors.As/errors.Is against the sentinel type.
var unknownBytes = []byte{
	0xC4, 0xC5, 0xC6, 0xC7, // gap between Float64(0xC1) and Int8(0xC8)
	0xCF,                   // gap after Bytes32(0xCE)
	0xD3,                   // gap between Str16(0xD1)/Str32(0xD2) and List8(0xD4)
	0xD7,                   // gap between List32(0xD6) and Map8(0xD8)
	0xDB, 0xDC, 0xDE, 0xDF, // gaps above Map32(0xDA) — 0xDB-0xEF not assigned
}

// TestUnknownTagReadValueError verifies that ReadValue returns a non-nil error
// (not a panic) when presented with every unrecognised marker byte.
//
// AC: decoder returns an error for every unknown tag; no panic.
func TestUnknownTagReadValueError(t *testing.T) {
	t.Parallel()

	for _, b := range unknownBytes {
		b := b
		t.Run("", func(t *testing.T) {
			t.Parallel()

			dec := packstream.NewDecoder(bytes.NewReader([]byte{b}))
			_, err := dec.ReadValue()
			if err == nil {
				t.Fatalf("byte 0x%02X: expected error, got nil", b)
			}
			if !strings.Contains(err.Error(), "packstream:") {
				t.Errorf("byte 0x%02X: error %q does not look like a packstream error", b, err.Error())
			}
		})
	}
}

// TestUnknownTagPeekTypeReturnsNull documents the current behaviour: PeekType
// returns TypeNull for unknown bytes because the lookup table defaults to the
// zero value. This means TypeNull is ambiguous — it covers both the real NULL
// marker (0xC0) and every unrecognised byte. A typed "TypeUnknown" sentinel
// does not currently exist.
//
// AC: PeekType reports TypeNull for the unknown byte (documents current behaviour).
func TestUnknownTagPeekTypeReturnsNull(t *testing.T) {
	t.Parallel()

	for _, b := range unknownBytes {
		b := b
		t.Run("", func(t *testing.T) {
			t.Parallel()

			dec := packstream.NewDecoder(bytes.NewReader([]byte{b}))
			got, err := dec.PeekType()
			if err != nil {
				t.Fatalf("byte 0x%02X: PeekType returned unexpected error: %v", b, err)
			}
			// TypeNull is the zero value; unknown bytes map here because the
			// lookup table defaults to zero for unrecognised entries. This is
			// not the same as detecting an unknown type; it is an ambiguity in
			// the current implementation.
			if got != packstream.TypeNull {
				t.Errorf("byte 0x%02X: want TypeNull (zero value for unknowns), got %v", b, got)
			}
		})
	}
}

// TestUnknownTagPoolInvariant verifies that after a pooled decoder returns an
// error for an unknown tag, the next Acquire returns a clean decoder ready to
// decode a valid value from fresh input.
//
// AC: pool invariant preserved — next Acquire returns a clean decoder.
func TestUnknownTagPoolInvariant(t *testing.T) {
	t.Parallel()

	pool := packstream.NewDecodePool()

	// Feed an unknown byte to a pooled decoder.
	badInput := bytes.NewReader([]byte{0xC4})
	dec := pool.Get(badInput)
	_, err := dec.ReadValue()
	if err == nil {
		t.Fatal("expected error from unknown byte, got nil")
	}
	pool.Put(dec)

	// Now acquire again and verify it handles a valid NULL cleanly.
	goodInput := bytes.NewReader([]byte{0xC0}) // markerNull
	dec2 := pool.Get(goodInput)
	defer pool.Put(dec2)

	got, err := dec2.ReadValue()
	if err != nil {
		t.Fatalf("clean decoder ReadValue(NULL): unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("clean decoder ReadValue(NULL): want nil, got %v", got)
	}
}

// TestUnknownTagNoPanic is an explicit no-panic guard. It drives ReadValue
// with the full set of unknown bytes sequentially in a single decoder,
// resetting it between calls.
//
// AC: no panic under any unknown byte.
func TestUnknownTagNoPanic(t *testing.T) {
	t.Parallel()

	dec := packstream.NewDecoder(bytes.NewReader(nil))

	for _, b := range unknownBytes {
		dec.Reset(bytes.NewReader([]byte{b}))
		// Must not panic.
		_, _ = dec.ReadValue()
	}
}
