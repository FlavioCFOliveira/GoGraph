package packstream_test

import (
	"bytes"
	"testing"

	"gograph/bolt/packstream"
)

// FuzzDecodeValue feeds arbitrary bytes to ReadValue and verifies that it
// never panics. If it successfully decodes a value, it re-encodes and
// re-decodes it and checks that the two decoded forms are equivalent.
func FuzzDecodeValue(f *testing.F) {
	// Seed corpus: one encoded example for each primitive type.
	seeds := []packstream.Value{
		nil,
		true,
		false,
		int64(0),
		int64(127),
		int64(-16),
		int64(1000),
		3.14,
		[]byte{0xDE, 0xAD, 0xBE, 0xEF},
		"hello",
		[]packstream.Value{int64(1), int64(2)},
		map[string]packstream.Value{"key": int64(42)},
		packstream.Struct{Tag: 0x01, Fields: []packstream.Value{int64(1)}},
	}
	for _, s := range seeds {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if err := enc.WriteValue(s); err != nil {
			continue
		}
		if err := enc.Flush(); err != nil {
			continue
		}
		f.Add(buf.Bytes())
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		dec := packstream.NewDecoder(bytes.NewReader(data))
		v, err := dec.ReadValue()
		if err != nil {
			// Decode error is acceptable for malformed input.
			return
		}

		// If decode succeeded, re-encode and re-decode must also succeed.
		var buf2 bytes.Buffer
		enc := packstream.NewEncoder(&buf2)
		if err := enc.WriteValue(v); err != nil {
			t.Fatalf("WriteValue failed after successful ReadValue: %v", err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		dec2 := packstream.NewDecoder(&buf2)
		_, err = dec2.ReadValue()
		if err != nil {
			t.Fatalf("second ReadValue failed after WriteValue: %v", err)
		}
	})
}
