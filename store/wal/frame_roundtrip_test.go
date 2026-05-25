package wal

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// TestFrame_RoundTrip_TableDriven encodes a Frame and immediately decodes it,
// asserting payload equality and that the decoded version matches
// CurrentVersion. The CRC is validated implicitly by Decode.
func TestFrame_RoundTrip_TableDriven(t *testing.T) {
	t.Parallel()

	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}

	large := make([]byte, 1<<20) // 1 MiB
	for i := range large {
		large[i] = byte(i & 0xff)
	}

	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: []byte{}},
		{name: "one_byte", payload: []byte{0x42}},
		{name: "five_bytes", payload: []byte{1, 2, 3, 4, 5}},
		{name: "large_1MB", payload: large},
		{name: "all_byte_values", payload: allBytes},
		{name: "unicode", payload: []byte("こんにちは GoGraph 🚀")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			n, err := Encode(&buf, Frame{Payload: tc.payload})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if want := HeaderSize + len(tc.payload); n != want {
				t.Fatalf("Encode returned n=%d, want %d", n, want)
			}

			got, err := Decode(&buf)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Version != CurrentVersion {
				t.Fatalf("Version = %d, want %d", got.Version, CurrentVersion)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Fatalf("Payload mismatch: got %d bytes, want %d bytes",
					len(got.Payload), len(tc.payload))
			}
		})
	}
}

// TestFrame_RoundTrip_Rapid is a property test: for any random payload up to
// 1024 bytes, Encode followed by Decode must return the original payload.
// CRC correctness is validated implicitly by Decode.
func TestFrame_RoundTrip_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		payload := rapid.SliceOfN(rapid.Byte(), 0, 1024).Draw(rt, "payload")

		var buf bytes.Buffer
		if _, err := Encode(&buf, Frame{Payload: payload}); err != nil {
			rt.Fatalf("Encode: %v", err)
		}
		got, err := Decode(&buf)
		if err != nil {
			rt.Fatalf("Decode: %v", err)
		}
		if !bytes.Equal(got.Payload, payload) {
			rt.Fatalf("payload mismatch: got %d bytes, want %d bytes",
				len(got.Payload), len(payload))
		}
	})
}
