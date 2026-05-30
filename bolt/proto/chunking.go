package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxChunkSize is the maximum number of payload bytes per Bolt chunk.
// Bolt's chunk length field is a uint16, so the theoretical maximum is 65535.
const maxChunkSize = 65535

// DefaultMaxMessageBytes is the default upper bound on the cumulative
// payload size of a single reassembled Bolt message. Chosen so that a
// Bolt message comfortably accommodates the largest realistic record
// projection (PackStream lists of strings, large maps from APOC-style
// procedures, multi-megabyte result rows) while keeping a single
// malicious client from coercing the server into multi-gigabyte
// allocations by streaming non-zero chunks indefinitely.
const DefaultMaxMessageBytes = 16 << 20 // 16 MiB

// ErrMessageTooLarge is returned by [ChunkedReader.ReadMessage] when
// the cumulative payload size of a single Bolt message would exceed
// the reader's MaxMessageBytes cap. Inspect with [errors.Is]. No
// partially read message is returned; the underlying reader is left
// positioned at the next byte after the offending chunk's payload so
// the caller may close the connection cleanly.
var ErrMessageTooLarge = errors.New("bolt chunk: cumulative message size exceeds MaxMessageBytes")

// ChunkedReader reassembles a complete Bolt message from a sequence of
// length-prefixed chunks read from an underlying buffered reader.
//
// Wire format (per chunk):
//
//	uint16 big-endian length  (0 = end-of-message sentinel)
//	<length bytes of payload>
//
// Bounded growth: every ChunkedReader carries a maxMessageBytes cap
// (configured via [NewChunkedReaderWithLimit]; defaults to
// [DefaultMaxMessageBytes] for [NewChunkedReader]). When the
// cumulative payload of a single message would exceed the cap,
// ReadMessage returns [ErrMessageTooLarge] before performing the
// would-be-oversized allocation. This closes the Slowloris-style DoS
// vector where a single client streams non-zero chunks until the
// server OOMs.
//
// ChunkedReader is NOT safe for concurrent use.
type ChunkedReader struct {
	r               *bufio.Reader
	maxMessageBytes int
}

// NewChunkedReader returns a ChunkedReader that reads from r with the
// [DefaultMaxMessageBytes] cap on cumulative message size. Use
// [NewChunkedReaderWithLimit] to set a different cap.
func NewChunkedReader(r io.Reader) *ChunkedReader {
	return NewChunkedReaderWithLimit(r, DefaultMaxMessageBytes)
}

// NewChunkedReaderWithLimit returns a ChunkedReader whose ReadMessage
// rejects any single Bolt message whose cumulative payload size would
// exceed maxMessageBytes with [ErrMessageTooLarge].
//
// A maxMessageBytes value of 0 or negative is replaced with
// [DefaultMaxMessageBytes]; the cap can never be disabled by an
// accidental zero-value configuration. Callers that genuinely want a
// very large bound should pass it explicitly.
func NewChunkedReaderWithLimit(r io.Reader, maxMessageBytes int) *ChunkedReader {
	if maxMessageBytes <= 0 {
		maxMessageBytes = DefaultMaxMessageBytes
	}
	return &ChunkedReader{r: bufio.NewReader(r), maxMessageBytes: maxMessageBytes}
}

// ReadMessage reads and reassembles one complete Bolt message.
//
// It reads chunks until it encounters a uint16(0) sentinel, appending each
// chunk's payload into a contiguous byte slice. The returned slice is freshly
// allocated and owned by the caller.
//
// Returns io.EOF when the underlying connection is closed cleanly before any
// bytes of the next message have arrived. Any other I/O error is wrapped and
// returned.
//
// Returns [ErrMessageTooLarge] when the cumulative payload of the message in
// flight would exceed the reader's MaxMessageBytes cap. The check is performed
// against the prospective total (current msg length + incoming chunkLen)
// before the would-be-oversized allocation is attempted, so a malicious
// client cannot coerce a single multi-gigabyte allocation by streaming
// non-zero chunks indefinitely.
func (cr *ChunkedReader) ReadMessage() ([]byte, error) {
	var header [2]byte
	var msg []byte

	for {
		// Read the 2-byte chunk length.
		_, err := io.ReadFull(cr.r, header[:])
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if msg == nil {
					// Clean close before any data: return io.EOF.
					return nil, io.EOF
				}
			}
			return nil, fmt.Errorf("bolt chunk: read length: %w", err)
		}

		chunkLen := int(binary.BigEndian.Uint16(header[:]))
		if chunkLen == 0 {
			// End-of-message sentinel.
			if msg == nil {
				msg = []byte{}
			}
			return msg, nil
		}
		if chunkLen > maxChunkSize {
			// This should never occur given the uint16 type, but guard defensively.
			return nil, fmt.Errorf("bolt chunk: chunk length %d exceeds maximum %d", chunkLen, maxChunkSize)
		}

		// Bound the cumulative size before the would-be-oversized
		// allocation. The check is len(msg)+chunkLen rather than just
		// len(msg) so a single chunk that lands exactly on the boundary
		// is accepted while a chunk that crosses it is rejected. Discard
		// the offending chunk's payload from the wire (best effort) so
		// the caller can close the connection without a half-consumed
		// chunk lingering in the kernel buffer.
		if len(msg)+chunkLen > cr.maxMessageBytes {
			if _, derr := io.CopyN(io.Discard, cr.r, int64(chunkLen)); derr != nil {
				return nil, fmt.Errorf("%w: drain offending chunk: %w", ErrMessageTooLarge, derr)
			}
			return nil, fmt.Errorf("%w: cap=%d, attempted=%d", ErrMessageTooLarge, cr.maxMessageBytes, len(msg)+chunkLen)
		}

		// Grow the message buffer and read exactly chunkLen bytes.
		offset := len(msg)
		msg = append(msg, make([]byte, chunkLen)...)
		if _, err := io.ReadFull(cr.r, msg[offset:]); err != nil {
			return nil, fmt.Errorf("bolt chunk: read payload: %w", err)
		}
	}
}

// ChunkedWriter frames a logical Bolt message into one or more chunks and
// writes them to the underlying buffered writer, followed by the uint16(0)
// end-of-message sentinel. It then flushes the buffer.
//
// ChunkedWriter is NOT safe for concurrent use.
type ChunkedWriter struct {
	w *bufio.Writer
}

// NewChunkedWriter returns a ChunkedWriter that writes to w.
func NewChunkedWriter(w io.Writer) *ChunkedWriter {
	return &ChunkedWriter{w: bufio.NewWriter(w)}
}

// WriteMessage writes msg as one or more Bolt chunks, appends the uint16(0)
// sentinel, and flushes the underlying writer.
//
// If msg is empty, WriteMessage writes only the sentinel (a valid, zero-length
// Bolt message).
func (cw *ChunkedWriter) WriteMessage(msg []byte) error {
	var lenBuf [2]byte

	// Write chunks of at most maxChunkSize bytes each.
	remaining := msg
	for len(remaining) > 0 {
		chunk := remaining
		if len(chunk) > maxChunkSize {
			chunk = chunk[:maxChunkSize]
		}
		remaining = remaining[len(chunk):]

		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(chunk))) //nolint:gosec // G115: chunk is capped to maxChunkSize (65535) two lines above, so len(chunk) <= 65535 and uint16 truncation cannot occur
		if _, err := cw.w.Write(lenBuf[:]); err != nil {
			return fmt.Errorf("bolt chunk: write length: %w", err)
		}
		if _, err := cw.w.Write(chunk); err != nil {
			return fmt.Errorf("bolt chunk: write payload: %w", err)
		}
	}

	// Write the end-of-message sentinel.
	binary.BigEndian.PutUint16(lenBuf[:], 0)
	if _, err := cw.w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("bolt chunk: write sentinel: %w", err)
	}

	return cw.w.Flush()
}
