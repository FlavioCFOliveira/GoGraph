package wal

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

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
