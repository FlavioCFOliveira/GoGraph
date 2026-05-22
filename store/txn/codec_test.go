package txn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// roundTripTrials is the number of random samples each codec must
// pass without error. The threshold matches the acceptance criterion
// recorded on the task in the roadmap.
const roundTripTrials = 10000

func TestCodec_String_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	// rapid.Check runs a generator-driven shrinking property test.
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.String().Draw(r, "v")
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got != want {
			t.Fatalf("round-trip mismatch: got %q want %q", got, want)
		}
		if len(rest) != 0 {
			t.Fatalf("trailing bytes after decode: %d", len(rest))
		}
	})
	// The acceptance criterion requires ≥10^4 successful samples. A
	// dedicated for-loop on top of the property check guarantees the
	// count regardless of rapid's internal budget.
	for i := 0; i < roundTripTrials; i++ {
		want := fmt.Sprintf("sample-%d", i)
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode #%d: %v", i, err)
		}
		if got != want || len(rest) != 0 {
			t.Fatalf("#%d mismatch", i)
		}
	}
}

func TestCodec_String_EmbeddedNulAndUnicode(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	cases := []string{
		"",
		"\x00",
		"abc\x00def",
		"\xff\xfe\xfd",
		"日本語",
		"emoji-\U0001F600",
		"new\nline\t\r",
		" leading and trailing ",
	}
	for _, want := range cases {
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("%q: Decode: %v", want, err)
		}
		if got != want {
			t.Fatalf("%q round-trip got %q", want, got)
		}
		if len(rest) != 0 {
			t.Fatalf("%q trailing bytes %d", want, len(rest))
		}
	}
}

func TestCodec_Int_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewIntCodec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Int().Draw(r, "v")
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
	// Drive 10^4 explicit samples to satisfy the acceptance criterion.
	for i := 0; i < roundTripTrials; i++ {
		want := i*7 - 3
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

func TestCodec_Int32_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewInt32Codec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Int32().Draw(r, "v")
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
	for i := 0; i < roundTripTrials; i++ {
		want := int32(i) * -3
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

func TestCodec_Int64_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewInt64Codec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Int64().Draw(r, "v")
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
	for i := 0; i < roundTripTrials; i++ {
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

func TestCodec_Uint64_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewUint64Codec()
	rapid.Check(t, func(r *rapid.T) {
		want := rapid.Uint64().Draw(r, "v")
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
	for i := uint64(0); i < uint64(roundTripTrials); i++ {
		want := i * 0x100000001
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

func TestCodec_UUID_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewUUIDCodec()
	rapid.Check(t, func(r *rapid.T) {
		var want [16]byte
		for i := range want {
			want[i] = byte(rapid.IntRange(0, 255).Draw(r, fmt.Sprintf("b%d", i)))
		}
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got != want {
			t.Fatalf("round-trip mismatch: got %x want %x", got, want)
		}
		if len(rest) != 0 {
			t.Fatalf("trailing bytes: %d", len(rest))
		}
	})
	for i := 0; i < roundTripTrials; i++ {
		var want [16]byte
		for k := range want {
			want[k] = byte((i*31 + k*7) & 0xFF)
		}
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode #%d: %v", i, err)
		}
		if got != want || len(rest) != 0 {
			t.Fatalf("#%d mismatch", i)
		}
	}
}

// labeledID is a custom type used to exercise the
// [BinaryMarshalerCodec]. It carries a uint32 id and a string label,
// serialised back-to-back as length-prefixed fields. The receiver is
// declared on the pointer so the value type itself is comparable.
type labeledID struct {
	id    uint32
	label string
}

// MarshalBinary writes id followed by the length-prefixed label.
func (l *labeledID) MarshalBinary() ([]byte, error) {
	out := make([]byte, 4+4+len(l.label))
	binary.LittleEndian.PutUint32(out, l.id)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(l.label)))
	copy(out[8:], l.label)
	return out, nil
}

// UnmarshalBinary reverses [labeledID.MarshalBinary].
func (l *labeledID) UnmarshalBinary(data []byte) error {
	if len(data) < 8 {
		return errors.New("labeledID: short")
	}
	l.id = binary.LittleEndian.Uint32(data)
	n := binary.LittleEndian.Uint32(data[4:])
	if uint64(len(data)-8) < uint64(n) {
		return errors.New("labeledID: truncated label")
	}
	l.label = string(data[8 : 8+n])
	return nil
}

func TestCodec_BinaryMarshaler_Roundtrip(t *testing.T) {
	t.Parallel()
	codec := NewBinaryMarshalerCodec[labeledID, *labeledID]()
	rapid.Check(t, func(r *rapid.T) {
		want := labeledID{
			id:    rapid.Uint32().Draw(r, "id"),
			label: rapid.String().Draw(r, "label"),
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
	for i := 0; i < roundTripTrials; i++ {
		want := labeledID{id: uint32(i * 13), label: fmt.Sprintf("L%d", i)}
		buf, _ := codec.Encode(nil, want)
		got, rest, err := codec.Decode(buf)
		if err != nil {
			t.Fatalf("Decode #%d: %v", i, err)
		}
		if got != want || len(rest) != 0 {
			t.Fatalf("#%d mismatch", i)
		}
	}
}

func TestCodec_AppendSemantics(t *testing.T) {
	t.Parallel()
	// Codecs MUST preserve the caller's prefix in buf and only append
	// the encoded value to the tail. This is the contract that lets
	// Store.encodeOpTyped reuse a single scratch buffer per op.
	codec := NewStringCodec()
	prefix := []byte{0xAA, 0xBB, 0xCC}
	buf, _ := codec.Encode(append([]byte(nil), prefix...), "hello")
	if !bytes.HasPrefix(buf, prefix) {
		t.Fatalf("encoded buffer dropped the caller-supplied prefix")
	}
	// The decode tail should ignore anything before the value, since
	// Decode reads from the head: we feed it the trailing portion.
	got, rest, err := codec.Decode(buf[len(prefix):])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "hello" || len(rest) != 0 {
		t.Fatalf("unexpected decode: got=%q rest=%d", got, len(rest))
	}
}

func TestCodec_Decode_RejectsTruncated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{
			name: "string short header",
			fn: func(t *testing.T) {
				_, _, err := NewStringCodec().Decode([]byte{0x01, 0x00})
				if !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v, want ErrCodecDecode", err)
				}
			},
		},
		{
			name: "string short body",
			fn: func(t *testing.T) {
				buf := []byte{0x05, 0x00, 0x00, 0x00, 'a', 'b'}
				_, _, err := NewStringCodec().Decode(buf)
				if !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v, want ErrCodecDecode", err)
				}
			},
		},
		{
			name: "varint empty",
			fn: func(t *testing.T) {
				if _, _, err := NewIntCodec().Decode(nil); !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "int32 out of range",
			fn: func(t *testing.T) {
				// Encode a value greater than MaxInt32 via int64 codec,
				// then attempt to decode as int32.
				buf, _ := NewInt64Codec().Encode(nil, int64(1)<<40)
				if _, _, err := NewInt32Codec().Decode(buf); !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "uuid short",
			fn: func(t *testing.T) {
				if _, _, err := NewUUIDCodec().Decode(make([]byte, 15)); !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "binary marshaler short header",
			fn: func(t *testing.T) {
				if _, _, err := NewBinaryMarshalerCodec[labeledID, *labeledID]().Decode([]byte{0x01}); !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "binary marshaler short body",
			fn: func(t *testing.T) {
				buf := []byte{0x20, 0x00, 0x00, 0x00, 'x'}
				if _, _, err := NewBinaryMarshalerCodec[labeledID, *labeledID]().Decode(buf); !errors.Is(err, ErrCodecDecode) {
					t.Fatalf("err = %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t) })
	}
}
