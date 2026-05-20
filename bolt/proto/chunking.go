package proto

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// maxChunkSize is the maximum number of payload bytes per Bolt chunk.
// Bolt's chunk length field is a uint16, so the theoretical maximum is 65535.
const maxChunkSize = 65535

// ChunkedReader reassembles a complete Bolt message from a sequence of
// length-prefixed chunks read from an underlying buffered reader.
//
// Wire format (per chunk):
//
//	uint16 big-endian length  (0 = end-of-message sentinel)
//	<length bytes of payload>
//
// ChunkedReader is NOT safe for concurrent use.
type ChunkedReader struct {
	r *bufio.Reader
}

// NewChunkedReader returns a ChunkedReader that reads from r.
func NewChunkedReader(r io.Reader) *ChunkedReader {
	return &ChunkedReader{r: bufio.NewReader(r)}
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

		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(chunk)))
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
