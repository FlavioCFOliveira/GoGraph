package wal

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// errReader returns the given error on the very first Read.
type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

// shortReader yields n bytes once then returns the configured error.
type shortReader struct {
	data []byte
	err  error
}

func (s *shortReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, s.err
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}

func TestDecode_PropagatesNonEOFReaderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("synthetic disk error")
	_, err := Decode(errReader{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Decode = %v, want %v", err, sentinel)
	}
}

func TestDecode_PropagatesNonEOFReaderErrorMidPayload(t *testing.T) {
	t.Parallel()
	// Build a valid header that claims plen=8 but then yield only 0
	// of those bytes with a synthetic non-EOF error. Decode must
	// propagate the underlying error verbatim (not convert to
	// ErrTornFrame), since synthetic errors are non-EOF.
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("12345678")}); err != nil {
		t.Fatal(err)
	}
	head := buf.Bytes()[:HeaderSize]
	sentinel := errors.New("disk slipped under our feet")
	sr := &shortReader{data: append([]byte(nil), head...), err: sentinel}
	_, err := Decode(sr)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Decode mid-payload = %v, want %v", err, sentinel)
	}
}

func TestDecode_TruncatedAtPayloadBoundary(t *testing.T) {
	t.Parallel()
	// A frame with a non-empty payload truncated *exactly* at the end
	// of the header: header is complete, payload is empty. Decode must
	// surface ErrTornFrame rather than mis-parsing.
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	head := buf.Bytes()[:HeaderSize]
	_, err := Decode(bytes.NewReader(head))
	if !errors.Is(err, ErrTornFrame) {
		t.Fatalf("Decode header-only = %v, want ErrTornFrame", err)
	}
}

func TestDecode_EmptyInputReturnsTornFrame(t *testing.T) {
	t.Parallel()
	_, err := Decode(bytes.NewReader(nil))
	if !errors.Is(err, ErrTornFrame) {
		t.Fatalf("Decode empty = %v, want ErrTornFrame", err)
	}
}

func TestDecode_CRCMismatchOnCorruptedPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Payload: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	frame := buf.Bytes()
	// Corrupt one byte of the payload; the CRC will no longer match.
	frame[HeaderSize+2] ^= 0xff
	_, err := Decode(bytes.NewReader(frame))
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("Decode corrupted payload = %v, want ErrCRCMismatch", err)
	}
}

// Sanity: io.EOF on the very first Read decodes as ErrTornFrame
// (the clean-EOF case is handled by the Reader; from the Decode
// perspective an EOF mid-header is always a torn frame).
func TestDecode_FirstReadEOFIsTorn(t *testing.T) {
	t.Parallel()
	_, err := Decode(errReader{err: io.EOF})
	if !errors.Is(err, ErrTornFrame) {
		t.Fatalf("Decode EOF = %v, want ErrTornFrame", err)
	}
}
