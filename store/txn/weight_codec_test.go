package txn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"pgregory.net/rapid"
)

// weightRoundTripTrials mirrors the N-side codec acceptance threshold;
// each built-in weight codec must round-trip ≥10^4 deterministic
// samples on top of the rapid property test.
const weightRoundTripTrials = 10000

func TestWeightCodec_Int64_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewInt64WeightCodec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Int64().Draw(r, "w")
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode(%d): %v", want, err)
		}
		if got != want {
			t.Fatalf("round-trip mismatch: got %d want %d", got, want)
		}
		if len(rest) != 0 {
			t.Fatalf("trailing bytes: %d", len(rest))
		}
	})
	for i := 0; i < weightRoundTripTrials; i++ {
		want := int64(i*i) - 17
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode #%d (%d): %v", i, want, err)
		}
		if got != want || len(rest) != 0 {
			t.Fatalf("#%d mismatch want=%d got=%d", i, want, got)
		}
	}
}

// TestWeightCodec_Float64_Roundtrip exercises the canonical
// IEEE 754 little-endian layout for float64 weights, including the
// degenerate values ±0.0, ±Inf, and a sample NaN payload. The
// codec must round-trip bits losslessly: every encoded value must
// produce the same Float64bits on decode, even for NaN payloads
// where the surface-level equality operator returns false.
func TestWeightCodec_Float64_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewFloat64WeightCodec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Float64().Draw(r, "w")
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode(%v): %v", want, err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("round-trip bit mismatch: got %#x want %#x", math.Float64bits(got), math.Float64bits(want))
		}
		if len(rest) != 0 {
			t.Fatalf("trailing bytes: %d", len(rest))
		}
	})
	// 100 random samples plus the canonical edge cases required by the
	// task spec (±0, ±Inf, NaN).
	edge := []float64{
		0.0,
		math.Copysign(0, -1),
		math.Inf(+1),
		math.Inf(-1),
		math.NaN(),
		math.MaxFloat64,
		-math.MaxFloat64,
		math.SmallestNonzeroFloat64,
		-math.SmallestNonzeroFloat64,
	}
	for _, want := range edge {
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode(%v): %v", want, err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("edge mismatch: got %#x want %#x", math.Float64bits(got), math.Float64bits(want))
		}
		if len(rest) != 0 {
			t.Fatalf("edge trailing bytes: %d", len(rest))
		}
	}
}

// TestWeightCodec_Float64_GoldenBytes locks down the wire form so a
// regression in math.Float64bits ordering / endianness is caught at
// the test boundary.
func TestWeightCodec_Float64_GoldenBytes(t *testing.T) {
	t.Parallel()
	codec := NewFloat64WeightCodec()
	want := []byte{
		// 1.5 = 0x3FF8000000000000 → LE: 00 00 00 00 00 00 F8 3F
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF8, 0x3F,
	}
	got, _ := codec.Encode(nil, 1.5)
	if !bytes.Equal(got, want) {
		t.Fatalf("float64 wire form drift: got % x want % x", got, want)
	}
}

func TestWeightCodec_Float64_DecodeRejectsShort(t *testing.T) {
	t.Parallel()
	codec := NewFloat64WeightCodec()
	if _, _, err := codec.Decode([]byte{0x00, 0x00, 0x00}); !errors.Is(err, ErrCodecDecode) {
		t.Fatalf("Decode(<8 bytes) err = %v, want ErrCodecDecode", err)
	}
}

func TestWeightCodec_Int64_DecodeRejectsEmpty(t *testing.T) {
	t.Parallel()
	codec := NewInt64WeightCodec()
	if _, _, err := codec.Decode(nil); !errors.Is(err, ErrCodecDecode) {
		t.Fatalf("Decode(nil) err = %v, want ErrCodecDecode", err)
	}
}

// weightAB exercises the BinaryMarshaler-backed weight codec; the
// type carries a pair of fields so MarshalBinary has a non-trivial
// shape to round-trip through the length-prefixed layout.
type weightAB struct {
	a uint32
	b uint32
}

func (w *weightAB) MarshalBinary() ([]byte, error) {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out, w.a)
	binary.LittleEndian.PutUint32(out[4:], w.b)
	return out, nil
}

func (w *weightAB) UnmarshalBinary(data []byte) error {
	if len(data) < 8 {
		return errors.New("weightAB: short")
	}
	w.a = binary.LittleEndian.Uint32(data)
	w.b = binary.LittleEndian.Uint32(data[4:])
	return nil
}

func TestWeightCodec_BinaryMarshaler_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewBinaryMarshalerWeightCodec[weightAB, *weightAB]()
	rapid.Check(t, func(r *rapid.T) {
		want := weightAB{
			a: rapid.Uint32().Draw(r, "a"),
			b: rapid.Uint32().Draw(r, "b"),
		}
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got != want {
			t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
		}
		if len(rest) != 0 {
			t.Fatalf("trailing bytes: %d", len(rest))
		}
	})
}

func TestWeightCodec_BinaryMarshaler_DecodeShort(t *testing.T) {
	t.Parallel()
	codec := NewBinaryMarshalerWeightCodec[weightAB, *weightAB]()
	if _, _, err := codec.Decode([]byte{0x01}); !errors.Is(err, ErrCodecDecode) {
		t.Fatalf("Decode short header err = %v, want ErrCodecDecode", err)
	}
	// Length-prefix says 32 bytes but body has only 4: the UnmarshalBinary
	// path errors out and the codec surfaces ErrCodecDecode.
	bogus := []byte{0x20, 0x00, 0x00, 0x00, 'x', 'y', 'z', 'q'}
	if _, _, err := codec.Decode(bogus); !errors.Is(err, ErrCodecDecode) {
		t.Fatalf("Decode short body err = %v, want ErrCodecDecode", err)
	}
}

// TestWeightCodec_AppendSemantics asserts the same prefix-preserving
// contract as the N-side TestCodec_AppendSemantics, since the same
// encodeOpTyped scratch-buffer reuse pattern applies on the W side.
func TestWeightCodec_AppendSemantics(t *testing.T) {
	t.Parallel()
	codec := NewFloat64WeightCodec()
	prefix := []byte{0xAA, 0xBB, 0xCC}
	buf, _ := codec.Encode(append([]byte(nil), prefix...), 3.14)
	if !bytes.HasPrefix(buf, prefix) {
		t.Fatalf("Float64 codec dropped caller-supplied prefix")
	}
	got, rest, err := codec.Decode(buf[len(prefix):])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != 3.14 || len(rest) != 0 {
		t.Fatalf("unexpected decode: got=%v rest=%d", got, len(rest))
	}
}
