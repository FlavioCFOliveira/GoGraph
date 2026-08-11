package proto_test

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// countingWriter records how many times Write was called, which is what a
// flush costs: bufio.Writer issues exactly one Write to its underlying writer
// per flush of a non-empty buffer, and that Write is the syscall when the
// underlying writer is a net.Conn.
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// TestChunkedWriter_AutoFlushDefaultsToOnAndFlushesPerMessage pins the default,
// because it is what every caller that has not thought about framing relies on
// — including the request side of the protocol, which writes one message and
// then waits for a reply that will never come if the message sits in a buffer.
func TestChunkedWriter_AutoFlushDefaultsToOnAndFlushesPerMessage(t *testing.T) {
	var cw countingWriter
	w := proto.NewChunkedWriter(&cw)

	const msgs = 8
	for i := 0; i < msgs; i++ {
		if err := w.WriteMessage([]byte{byte(i)}); err != nil {
			t.Fatalf("WriteMessage %d: %v", i, err)
		}
		if cw.writes != i+1 {
			t.Fatalf("after %d messages: got %d underlying writes, want %d "+
				"(auto-flush must deliver each message on its own)",
				i+1, cw.writes, i+1)
		}
	}
}

// TestChunkedWriter_AutoFlushOffBatchesMessagesIntoOneWrite is the regression
// test for the defect measured in docs/cpu-vs-neo4j-memgraph-2026-08-11.md §4:
// a K-row Bolt result cost K write(2) syscalls because WriteMessage flushed
// unconditionally, so the buffer in front of the connection never accumulated
// anything.
//
// The assertion is deliberately on the WRITE COUNT rather than on elapsed time.
// A timing assertion would be a load question; the syscall count is the defect.
func TestChunkedWriter_AutoFlushOffBatchesMessagesIntoOneWrite(t *testing.T) {
	var cw countingWriter
	w := proto.NewChunkedWriter(&cw)
	w.SetAutoFlush(false)

	// Small messages, so that many of them fit inside one bufio buffer and the
	// batching is attributable to auto-flush being off rather than to the
	// buffer happening to be large enough for one message.
	const msgs = 64
	for i := 0; i < msgs; i++ {
		if err := w.WriteMessage([]byte{byte(i)}); err != nil {
			t.Fatalf("WriteMessage %d: %v", i, err)
		}
	}
	if cw.writes != 0 {
		t.Errorf("before Flush: got %d underlying writes, want 0 "+
			"(with auto-flush off, buffered messages must not reach the writer yet)",
			cw.writes)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if cw.writes != 1 {
		t.Errorf("after Flush: got %d underlying writes, want 1 "+
			"(%d small messages must be delivered in one write)", cw.writes, msgs)
	}
}

// TestChunkedWriter_AutoFlushDoesNotChangeTheBytesOnTheWire proves the change
// is invisible to the peer: batching may only alter WHEN bytes are written,
// never WHAT is written. Without this, a framing regression could hide behind
// the write-count assertions above.
func TestChunkedWriter_AutoFlushDoesNotChangeTheBytesOnTheWire(t *testing.T) {
	msgs := [][]byte{
		nil,                                // the empty message: sentinel only
		{0x01},                             // one byte
		bytes.Repeat([]byte{0xAB}, 10),     // a short payload
		bytes.Repeat([]byte{0xCD}, 70_000), // spans more than one 65535-byte chunk
	}

	var on countingWriter
	wOn := proto.NewChunkedWriter(&on)
	for _, m := range msgs {
		if err := wOn.WriteMessage(m); err != nil {
			t.Fatalf("auto-flush on: %v", err)
		}
	}

	var off countingWriter
	wOff := proto.NewChunkedWriter(&off)
	wOff.SetAutoFlush(false)
	for _, m := range msgs {
		if err := wOff.WriteMessage(m); err != nil {
			t.Fatalf("auto-flush off: %v", err)
		}
	}
	if err := wOff.Flush(); err != nil {
		t.Fatalf("auto-flush off: Flush: %v", err)
	}

	if !bytes.Equal(on.buf.Bytes(), off.buf.Bytes()) {
		t.Fatalf("byte streams differ: auto-flush on produced %d bytes, off produced %d",
			on.buf.Len(), off.buf.Len())
	}
	if off.writes >= on.writes {
		t.Errorf("auto-flush off issued %d writes and on issued %d; "+
			"off must issue strictly fewer", off.writes, on.writes)
	}
}

// TestChunkedWriter_FlushIsSafeWhenNothingIsBuffered guards the server's
// unconditional flush on every non-RECORD message: it runs even when the
// previous message already drained the buffer.
func TestChunkedWriter_FlushIsSafeWhenNothingIsBuffered(t *testing.T) {
	var cw countingWriter
	w := proto.NewChunkedWriter(&cw)
	w.SetAutoFlush(false)

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush on an empty buffer: %v", err)
	}
	if cw.writes != 0 {
		t.Errorf("Flush on an empty buffer issued %d writes, want 0", cw.writes)
	}
	if err := w.WriteMessage([]byte{0x01}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if cw.writes != 1 {
		t.Errorf("got %d writes, want 1: a redundant Flush must not write again", cw.writes)
	}
}
