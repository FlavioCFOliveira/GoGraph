package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
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
// Aggregate bound: maxMessageBytes caps ONE connection's reassembly
// buffer, but the sum across all connections is only implicitly bounded
// by MaxConnections × maxMessageBytes. A ChunkedReader with an
// [packstream.InboundBudget] attached via [ChunkedReader.SetInboundBudget]
// charges the transient reassembly buffer's bytes against that shared,
// engine-wide ceiling as the buffer grows and releases them symmetrically
// once the message is assembled — so the total reassembly memory in flight
// across every connection is centrally bounded, sharing one pool with the
// decoder's inbound-decode accounting. A nil/disabled budget leaves the
// reader bounded only by maxMessageBytes.
//
// ChunkedReader is NOT safe for concurrent use. The attached
// [packstream.InboundBudget], however, IS safe for concurrent use: one
// budget is shared by every connection's reader (and decoder) at once.
type ChunkedReader struct {
	r *bufio.Reader
	// budget is the engine-wide inbound-memory ceiling the reassembly buffer
	// is charged against, or nil when no (enabled) budget is attached, in
	// which case reassembly is bounded only by maxMessageBytes. Set via
	// SetInboundBudget. See ChunkedReader's type doc.
	budget          *packstream.InboundBudget
	maxMessageBytes int
	// hdr is the scratch space for the 2-byte big-endian chunk-length header
	// read at the top of every chunk iteration. It is a field rather than a
	// ReadMessage local because io.ReadFull takes an io.Reader INTERFACE, so a
	// local [2]byte cannot stay on the stack: escape analysis reports
	// "moved to heap: header", costing one heap object per inbound message.
	// Hoisting it here pays that allocation once per reader instead. Sound
	// because ChunkedReader is documented NOT safe for concurrent use, and
	// because the header is fully consumed (decoded to chunkLen) before the
	// next read overwrites it — it is never aliased by a returned message.
	hdr [2]byte
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

// SetInboundBudget attaches the engine-wide inbound-memory ceiling that this
// reader charges its transient message-reassembly buffer against, so aggregate
// reassembly memory across all connections shares one pool with the decoder's
// inbound-decode accounting (a per-Server DoS bound, CWE-770). Pass the Server's
// shared [packstream.InboundBudget]; a nil or disabled budget detaches the
// accounting, leaving the reader bounded only by its maxMessageBytes cap.
//
// It is the reassembly-side counterpart of
// [github.com/FlavioCFOliveira/GoGraph/bolt/packstream.Decoder.SetInboundBudget].
// Call it once, immediately after construction and before the first
// [ChunkedReader.ReadMessage]; the reader draws on the budget for the lifetime
// of every subsequent reassembly and releases symmetrically. A disabled budget
// is stored as nil so ReadMessage pays no per-chunk accounting cost when the
// operator has not opted into an inbound-memory ceiling.
func (cr *ChunkedReader) SetInboundBudget(b *packstream.InboundBudget) {
	if b.Enabled() {
		cr.budget = b
	} else {
		cr.budget = nil
	}
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
// A standalone uint16(0) chunk that does NOT terminate an in-progress message
// (i.e. a 00 00 arriving with no preceding payload chunk) is a Bolt 4.1+ NOOP
// keep-alive: per the Bolt Protocol Manual a conformant peer MUST silently
// discard it (zero PackStream bytes, never decoded, never answered). Such a
// NOOP is skipped here at the framing layer — ReadMessage loops past it and
// reads the next real message — so the serve loop never sees a spurious
// zero-length message to (mis)decode. A uint16(0) that legitimately TERMINATES
// a non-empty message is ordinary end-of-message chunking and is unaffected.
// Skipping the NOOP at this layer (rather than in the serve loop) keeps
// chunk-reassembly self-contained: the reader's contract becomes "every
// returned message has at least one payload byte", which matches real Bolt
// messages (always at least a struct header byte) and leaves callers free of
// NOOP-awareness.
//
// Returns [ErrMessageTooLarge] when the cumulative payload of the message in
// flight would exceed the reader's MaxMessageBytes cap. The check is performed
// against the prospective total (current msg length + incoming chunkLen)
// before the would-be-oversized allocation is attempted, so a malicious
// client cannot coerce a single multi-gigabyte allocation by streaming
// non-zero chunks indefinitely.
func (cr *ChunkedReader) ReadMessage() ([]byte, error) {
	var msg []byte

	// charged tracks the bytes this call has reserved from the shared inbound
	// budget for the growing reassembly buffer. The buffer is transient — it
	// belongs to the reassembly phase — so the reservation is released
	// symmetrically once the message is assembled (or the read aborts), on every
	// return path, via this deferred release. cr.budget is nil unless an enabled
	// budget was attached (SetInboundBudget), so an unbudgeted reader charges,
	// releases, and pays nothing.
	var charged int64
	defer func() {
		if charged > 0 {
			cr.budget.Release(charged)
		}
	}()

	for {
		// Read the 2-byte chunk length.
		_, err := io.ReadFull(cr.r, cr.hdr[:])
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if msg == nil {
					// Clean close before any data: return io.EOF.
					return nil, io.EOF
				}
			}
			return nil, fmt.Errorf("bolt chunk: read length: %w", err)
		}

		chunkLen := int(binary.BigEndian.Uint16(cr.hdr[:]))
		if chunkLen == 0 {
			if msg == nil {
				// Standalone 00 00 with no in-progress message body: a Bolt
				// 4.1+ NOOP keep-alive. Silently discard it and read the next
				// message rather than returning a (spurious) zero-length
				// message the serve loop would fail to decode.
				continue
			}
			// End-of-message sentinel terminating a real (non-empty) message.
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

		// Charge the incoming chunk's bytes against the engine-wide inbound
		// budget BEFORE growing the buffer, so aggregate reassembly memory in
		// flight across every connection is centrally bounded rather than merely
		// capped at MaxConnections × maxMessageBytes. A nil (absent/disabled)
		// budget makes this a no-op. When the shared pool cannot satisfy the
		// charge the server is under aggregate inbound-memory pressure: drain the
		// offending chunk (best effort, mirroring the too-large path so the
		// caller can close the connection without a half-consumed chunk lingering
		// in the kernel buffer) and reject with [packstream.ErrInboundBudgetExceeded]
		// BEFORE the would-be allocation. The partial charge already taken for
		// this message is returned by the deferred release above.
		if cr.budget != nil {
			if !cr.budget.TryReserve(int64(chunkLen)) {
				if _, derr := io.CopyN(io.Discard, cr.r, int64(chunkLen)); derr != nil {
					return nil, fmt.Errorf("%w: drain offending chunk: %w", packstream.ErrInboundBudgetExceeded, derr)
				}
				return nil, fmt.Errorf("%w: reassembly buffer (cap=%d, in-flight=%d, chunk=%d)", packstream.ErrInboundBudgetExceeded, cr.maxMessageBytes, len(msg), chunkLen)
			}
			charged += int64(chunkLen)
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
// end-of-message sentinel. By default it then flushes the buffer, so one
// WriteMessage call reaches the peer on its own; see [ChunkedWriter.SetAutoFlush]
// for the streaming case.
//
// ChunkedWriter is NOT safe for concurrent use.
type ChunkedWriter struct {
	w *bufio.Writer
	// autoFlush reports whether WriteMessage flushes on return. It defaults to
	// true so that a caller which writes one message and then waits for the
	// peer — the request side of the protocol, and any caller that has not
	// thought about framing — is correct without doing anything.
	autoFlush bool
}

// NewChunkedWriter returns a ChunkedWriter that writes to w and flushes after
// every message.
func NewChunkedWriter(w io.Writer) *ChunkedWriter {
	return &ChunkedWriter{w: bufio.NewWriter(w), autoFlush: true}
}

// SetAutoFlush controls whether [ChunkedWriter.WriteMessage] flushes the
// underlying writer before returning. It defaults to true.
//
// Disabling it lets a run of messages accumulate in the buffer and reach the
// peer in a small number of writes instead of one write per message, which is
// what a result stream wants: a K-row result otherwise costs K write syscalls,
// and the buffer in front of the connection can never do its job. Measured on
// the Bolt server, a 1 000-row result cost 1 953 µs of CPU with a flush per
// message and 324 µs without one (docs/cpu-vs-neo4j-memgraph-2026-08-11.md §4).
//
// A caller that disables auto-flush TAKES OVER responsibility for delivery: it
// MUST call [ChunkedWriter.Flush] before it blocks waiting for the peer, or the
// buffered messages will never be sent and the exchange deadlocks.
func (cw *ChunkedWriter) SetAutoFlush(on bool) { cw.autoFlush = on }

// Flush writes any buffered bytes to the underlying writer. It is a no-op when
// the buffer is empty, so it is safe to call unconditionally.
func (cw *ChunkedWriter) Flush() error { return cw.w.Flush() }

// WriteMessage writes msg as one or more Bolt chunks and appends the uint16(0)
// sentinel. It flushes the underlying writer unless auto-flush has been
// disabled with [ChunkedWriter.SetAutoFlush].
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

	if cw.autoFlush {
		return cw.w.Flush()
	}
	return nil
}
