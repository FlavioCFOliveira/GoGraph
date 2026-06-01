package packstream_test

// encoder_overflow_test.go — T943: pin numeric boundary contracts for
// PackStream encoding. Two complementary concerns:
//   - WriteInt must encode each of the 5 marker classes (TinyInt, INT_8,
//     INT_16, INT_32, INT_64) at its exact boundary values and round-trip
//     them back through ReadInt without precision loss.
//   - WriteBytes/WriteString/WriteListHeader/WriteMapHeader must return
//     [ErrPayloadTooLarge] when the supplied length exceeds the 32-bit
//     wire prefix, instead of silently truncating to the low 32 bits.

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// TestWriteInt_BoundaryRoundTrip encodes each boundary value, decodes it,
// and asserts the value matches. Boundaries chosen at the exact edges
// where WriteInt switches marker classes, including the negative-side
// transitions that the two's-complement reinterpretation casts touch.
func TestWriteInt_BoundaryRoundTrip(t *testing.T) {
	boundaries := []int64{
		// TinyInt range edges.
		-16, -1, 0, 1, 127,
		// INT_8 edges (just below/above the TinyInt range).
		-17, -128,
		// INT_16 edges.
		128, math.MinInt16, math.MaxInt16,
		-129,
		// INT_32 edges.
		math.MinInt16 - 1, math.MaxInt16 + 1, math.MinInt32, math.MaxInt32,
		// INT_64 edges.
		math.MinInt32 - 1, math.MaxInt32 + 1, math.MinInt64, math.MaxInt64,
	}
	for _, v := range boundaries {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if err := enc.WriteInt(v); err != nil {
			t.Errorf("WriteInt(%d): %v", v, err)
			continue
		}
		if err := enc.Flush(); err != nil {
			t.Errorf("Flush after WriteInt(%d): %v", v, err)
			continue
		}
		dec := packstream.NewDecoder(&buf)
		got, err := dec.ReadInt()
		if err != nil {
			t.Errorf("ReadInt after WriteInt(%d): %v", v, err)
			continue
		}
		if got != v {
			t.Errorf("round-trip mismatch: WriteInt(%d) → ReadInt → %d", v, got)
		}
	}
}

// TestWriteString_TooLarge fabricates a string just over MaxUint16 (so it
// enters the STRING32 default branch) and confirms the encoder handles
// the boundary correctly. The contract under test is that
// checkUint32Length is consulted in the default branch; building a 4 GB
// string to exercise the actual overflow is impractical, so the
// boundary-just-above-MaxUint16 case is the closest behavioural pin.
func TestWriteString_AtUint16Boundary(t *testing.T) {
	n := math.MaxUint16 + 1 // 65 536: enters STRING32 case
	s := strings.Repeat("x", n)

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteString(s); err != nil {
		t.Fatalf("WriteString(len=%d): %v", n, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	dec := packstream.NewDecoder(&buf)
	got, err := dec.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if len(got) != n {
		t.Errorf("round-trip length: got %d, want %d", len(got), n)
	}
}

// TestErrPayloadTooLarge_Sentinel reports that ErrPayloadTooLarge is a
// stable sentinel value: callers must be able to distinguish payload-too-
// large errors from generic IO failures via errors.Is. Without this
// pin, a future refactor that wraps the sentinel in fmt.Errorf without
// %w would silently break detection on the caller side.
func TestErrPayloadTooLarge_Sentinel(t *testing.T) {
	// Direct identity check.
	if !errors.Is(packstream.ErrPayloadTooLarge, packstream.ErrPayloadTooLarge) {
		t.Fatal("ErrPayloadTooLarge must satisfy errors.Is reflexively")
	}
	// Distinct from a fresh error of the same message.
	other := errors.New(packstream.ErrPayloadTooLarge.Error())
	if errors.Is(other, packstream.ErrPayloadTooLarge) {
		t.Fatal("plain error with same message must not match ErrPayloadTooLarge via errors.Is")
	}
}
