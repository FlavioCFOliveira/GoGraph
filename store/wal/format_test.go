package wal

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// TestEncode_GoldenBytes pins the exact on-disk byte stream Encode produces
// for a known frame. It is the byte-identity gate for #1509 (header written
// from a stack array via two Writes instead of a single concatenated make):
// the golden vectors below were captured from the pre-#1509 single-buffer
// implementation and independently cross-checked against a from-scratch
// CRC32C(Castagnoli) over magic+version+length+payload. Any future change to
// the framing layout, the CRC input, or byte order will fail here, protecting
// the WAL's backward compatibility with archives written by earlier releases.
func TestEncode_GoldenBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
		want    []byte
	}{
		{
			name:    "payload",
			payload: []byte("hello, gograph"),
			want: []byte{
				0x47, 0x47, 0x57, 0x41, // magic GGWA
				0x01, 0x00, // version 1 (LE)
				0x0e, 0x00, 0x00, 0x00, // length 14 (LE)
				0xb2, 0x04, 0x13, 0x80, // crc32c (LE)
				0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x2c, 0x20, // "hello, "
				0x67, 0x6f, 0x67, 0x72, 0x61, 0x70, 0x68, // "gograph"
			},
		},
		{
			name:    "empty",
			payload: nil,
			want: []byte{
				0x47, 0x47, 0x57, 0x41, // magic GGWA
				0x01, 0x00, // version 1 (LE)
				0x00, 0x00, 0x00, 0x00, // length 0 (LE)
				0xde, 0xba, 0xa0, 0x2a, // crc32c (LE)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			n, err := Encode(&buf, Frame{Payload: tc.payload})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if n != len(tc.want) {
				t.Fatalf("Encode wrote %d bytes, want %d", n, len(tc.want))
			}
			if !bytes.Equal(buf.Bytes(), tc.want) {
				t.Fatalf("frame bytes mismatch:\n got %#v\nwant %#v", buf.Bytes(), tc.want)
			}
			// The golden frame must still decode cleanly — byte-identity and
			// round-trip are independent guarantees.
			out, err := Decode(bytes.NewReader(tc.want))
			if err != nil {
				t.Fatalf("Decode golden: %v", err)
			}
			// bytes.Equal treats nil and a zero-length slice as equal, so the
			// empty-payload case (tc.payload == nil, out.Payload == []byte{})
			// compares equal without a special case.
			if !bytes.Equal(out.Payload, tc.payload) {
				t.Fatalf("Decode golden payload = %q, want %q", out.Payload, tc.payload)
			}
		})
	}
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	t.Parallel()
	in := Frame{Payload: []byte("hello, gograph")}
	var buf bytes.Buffer
	n, err := Encode(&buf, in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if n != HeaderSize+len(in.Payload) {
		t.Fatalf("Encode wrote %d, want %d", n, HeaderSize+len(in.Payload))
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", out.Version, CurrentVersion)
	}
	if !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("Payload mismatch: %q vs %q", out.Payload, in.Payload)
	}
}

func TestEncodeDecode_EmptyPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out.Payload) != 0 {
		t.Fatalf("Payload = %v, want empty", out.Payload)
	}
}

func TestDecode_TornFrame(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("payload")}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	full := buf.Bytes()
	// Truncate at every possible offset and ensure no panic and a
	// reported error.
	for cut := 0; cut < len(full); cut++ {
		r := bytes.NewReader(full[:cut])
		_, err := Decode(r)
		if err == nil {
			t.Fatalf("cut=%d: expected error", cut)
		}
		if !errors.Is(err, ErrTornFrame) && !errors.Is(err, ErrBadMagic) && !errors.Is(err, ErrCRCMismatch) {
			t.Fatalf("cut=%d: unexpected error %v", cut, err)
		}
	}
}

func TestDecode_BadMagic(t *testing.T) {
	t.Parallel()
	buf := bytes.NewReader([]byte("XXXX\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	if _, err := Decode(buf); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

func TestDecode_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Version: CurrentVersion + 99, Payload: nil}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := Decode(&buf); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestDecode_CRCCorruption(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("important")}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	bs := buf.Bytes()
	r := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic test RNG
	for trial := 0; trial < 100; trial++ {
		dup := append([]byte(nil), bs...)
		// Flip a random bit somewhere in the frame.
		pos := r.IntN(len(dup))
		bit := byte(1 << r.IntN(8))
		dup[pos] ^= bit
		_, err := Decode(bytes.NewReader(dup))
		if err == nil {
			t.Fatalf("flip at pos=%d should not decode cleanly", pos)
		}
	}
}

func TestDecode_StreamOfFrames(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	payloads := [][]byte{
		[]byte("first"),
		[]byte("second message"),
		[]byte(""),
		[]byte("the last frame"),
	}
	for _, p := range payloads {
		if _, err := Encode(&buf, Frame{Payload: p}); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	for i, p := range payloads {
		out, err := Decode(&buf)
		if err != nil {
			t.Fatalf("frame %d Decode: %v", i, err)
		}
		if !bytes.Equal(out.Payload, p) {
			t.Fatalf("frame %d payload = %q, want %q", i, out.Payload, p)
		}
	}
	if _, err := Decode(&buf); !errors.Is(err, ErrTornFrame) {
		t.Fatalf("after stream: expected ErrTornFrame, got %v", err)
	}
}

func BenchmarkEncode(b *testing.B) {
	payload := make([]byte, 4096) // ~4 KB frame
	var buf bytes.Buffer
	buf.Grow((HeaderSize + 4096) * b.N)
	b.ReportAllocs()
	b.SetBytes(int64(HeaderSize + len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		_, _ = Encode(&buf, Frame{Payload: payload})
	}
}

func BenchmarkDecode(b *testing.B) {
	payload := make([]byte, 4096)
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: payload}); err != nil {
		b.Fatal(err)
	}
	frame := buf.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decode(bytes.NewReader(frame))
	}
}

var _ = io.EOF
