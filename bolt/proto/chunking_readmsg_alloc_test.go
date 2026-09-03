package proto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// ── t2716 evidence harness ────────────────────────────────────────────────
//
// Measures allocations per INBOUND MESSAGE for ChunkedReader.ReadMessage, and
// A/B-compares the shipped `append(msg, make([]byte, chunkLen)...)` idiom
// against the proposed `slices.Grow(...)[:offset+chunkLen]` rewrite. Both
// variants live in ONE binary so the comparison is interleaved by the testing
// package rather than split across two builds (no layout/ASLR drift).

// cycleReader replays buf forever with zero allocations, so the benchmark
// measures ReadMessage and nothing else.
type cycleReader struct {
	buf []byte
	pos int
}

func (c *cycleReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if c.pos == len(c.buf) {
			c.pos = 0
		}
		m := copy(p[n:], c.buf[c.pos:])
		c.pos += m
		n += m
	}
	return n, nil
}

// buildMessage frames payload into chunks of at most chunkSize bytes followed
// by the uint16(0) end-of-message sentinel.
func buildMessage(payload []byte, chunkSize int) []byte {
	var out []byte
	for off := 0; off < len(payload); {
		n := min(chunkSize, len(payload)-off)
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(n))
		out = append(out, hdr[:]...)
		out = append(out, payload[off:off+n]...)
		off += n
	}
	return append(out, 0, 0)
}

// readMessageGrow is the PROPOSED rewrite of ReadMessage: byte-for-byte the
// shipped function except that the throwaway make is replaced by slices.Grow
// plus an explicit reslice. Test-only; never linked into production.
func (cr *ChunkedReader) readMessageGrow() ([]byte, error) {
	var header [2]byte
	var msg []byte
	var charged int64
	defer func() {
		if charged > 0 {
			cr.budget.Release(charged)
		}
	}()

	for {
		_, err := io.ReadFull(cr.r, header[:])
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if msg == nil {
					return nil, io.EOF
				}
			}
			return nil, fmt.Errorf("bolt chunk: read length: %w", err)
		}

		chunkLen := int(binary.BigEndian.Uint16(header[:]))
		if chunkLen == 0 {
			if msg == nil {
				continue
			}
			return msg, nil
		}
		if chunkLen > maxChunkSize {
			return nil, fmt.Errorf("bolt chunk: chunk length %d exceeds maximum %d", chunkLen, maxChunkSize)
		}
		if len(msg)+chunkLen > cr.maxMessageBytes {
			if _, derr := io.CopyN(io.Discard, cr.r, int64(chunkLen)); derr != nil {
				return nil, fmt.Errorf("%w: drain offending chunk: %w", ErrMessageTooLarge, derr)
			}
			return nil, fmt.Errorf("%w: cap=%d, attempted=%d", ErrMessageTooLarge, cr.maxMessageBytes, len(msg)+chunkLen)
		}
		if cr.budget != nil {
			if !cr.budget.TryReserve(int64(chunkLen)) {
				if _, derr := io.CopyN(io.Discard, cr.r, int64(chunkLen)); derr != nil {
					return nil, fmt.Errorf("%w: drain offending chunk: %w", packstream.ErrInboundBudgetExceeded, derr)
				}
				return nil, fmt.Errorf("%w: reassembly buffer (cap=%d, in-flight=%d, chunk=%d)", packstream.ErrInboundBudgetExceeded, cr.maxMessageBytes, len(msg), chunkLen)
			}
			charged += int64(chunkLen)
		}

		// PROPOSED: grow capacity, then extend length without zeroing.
		offset := len(msg)
		msg = slices.Grow(msg, chunkLen)[:offset+chunkLen]
		if _, err := io.ReadFull(cr.r, msg[offset:]); err != nil {
			return nil, fmt.Errorf("bolt chunk: read payload: %w", err)
		}
	}
}

func benchReadMessage(b *testing.B, payloadLen, chunkSize int, grow bool) {
	b.Helper()
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i%251) + 1 // non-zero, so zero-fill bugs are visible
	}
	wire := buildMessage(payload, chunkSize)
	cr := &ChunkedReader{r: bufio.NewReader(&cycleReader{buf: wire}), maxMessageBytes: DefaultMaxMessageBytes}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var got []byte
		var err error
		if grow {
			got, err = cr.readMessageGrow()
		} else {
			got, err = cr.ReadMessage()
		}
		if err != nil {
			b.Fatalf("ReadMessage: %v", err)
		}
		if len(got) != payloadLen {
			b.Fatalf("len = %d, want %d", len(got), payloadLen)
		}
		sinkMsg = got
	}
}

var sinkMsg []byte

// 1 chunk per message — the dominant production shape (a RUN/PULL message is
// far below the 65535-byte chunk ceiling).
func BenchmarkReadMessage_1chunk_A_appendMake(b *testing.B) {
	benchReadMessage(b, 512, 65535, false)
}
func BenchmarkReadMessage_1chunk_B_slicesGrow(b *testing.B) {
	benchReadMessage(b, 512, 65535, true)
}

// 4 chunks per message — the shape the task's "4 messages each" evidence used.
func BenchmarkReadMessage_4chunk_A_appendMake(b *testing.B) {
	benchReadMessage(b, 4*4096, 4096, false)
}
func BenchmarkReadMessage_4chunk_B_slicesGrow(b *testing.B) {
	benchReadMessage(b, 4*4096, 4096, true)
}

// Large multi-chunk message: 16 chunks of 65535 bytes (~1 MiB).
func BenchmarkReadMessage_16chunk_A_appendMake(b *testing.B) {
	benchReadMessage(b, 16*65535, 65535, false)
}
func BenchmarkReadMessage_16chunk_B_slicesGrow(b *testing.B) {
	benchReadMessage(b, 16*65535, 65535, true)
}

// TestReadMessageGrowByteIdentity proves the two implementations return
// byte-identical messages across a wide spread of payload sizes and chunk
// splits — the wire-bytes-unchanged guarantee.
func TestReadMessageGrowByteIdentity(t *testing.T) {
	sizes := []int{1, 2, 15, 255, 256, 511, 512, 513, 1024, 4095, 4096, 65535, 65536, 131071, 200000}
	chunks := []int{1, 7, 255, 4096, 65535}
	for _, size := range sizes {
		for _, cs := range chunks {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i%251) + 1 // never zero
			}
			wire := buildMessage(payload, cs)

			a := NewChunkedReader(bytes.NewReader(wire))
			gotA, errA := a.ReadMessage()
			bb := NewChunkedReader(bytes.NewReader(wire))
			gotB, errB := bb.readMessageGrow()

			if (errA == nil) != (errB == nil) {
				t.Fatalf("size=%d chunk=%d: errA=%v errB=%v", size, cs, errA, errB)
			}
			if errA != nil {
				continue
			}
			if !bytes.Equal(gotA, gotB) {
				t.Fatalf("size=%d chunk=%d: BYTES DIFFER", size, cs)
			}
			if !bytes.Equal(gotA, payload) {
				t.Fatalf("size=%d chunk=%d: shipped impl does not round-trip payload", size, cs)
			}
		}
	}
}

// ── Part 2 ceiling: what buffer REUSE would actually buy ─────────────────
//
// readMessageReuse is the most favourable possible reuse variant: a single
// per-reader buffer, reset to zero length each call and grown in place. It is
// UNSOUND for production (the returned slice aliases the reader's buffer and
// crosses a channel to the dispatch goroutine, which would observe the NEXT
// message's bytes) and exists ONLY to establish the upper bound on the prize
// that any sound reuse scheme — double-buffering, pooling — could approach but
// never exceed.
func (cr *ChunkedReader) readMessageReuse(reuse *[]byte) ([]byte, error) {
	var header [2]byte
	msg := (*reuse)[:0]

	for {
		_, err := io.ReadFull(cr.r, header[:])
		if err != nil {
			return nil, err
		}
		chunkLen := int(binary.BigEndian.Uint16(header[:]))
		if chunkLen == 0 {
			if len(msg) == 0 {
				continue
			}
			*reuse = msg
			return msg, nil
		}
		if len(msg)+chunkLen > cr.maxMessageBytes {
			return nil, ErrMessageTooLarge
		}
		offset := len(msg)
		if cap(msg) < offset+chunkLen {
			msg = slices.Grow(msg, chunkLen)
		}
		msg = msg[:offset+chunkLen]
		if _, err := io.ReadFull(cr.r, msg[offset:]); err != nil {
			return nil, err
		}
	}
}

func benchReadMessageReuse(b *testing.B, payloadLen, chunkSize int) {
	b.Helper()
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i%251) + 1
	}
	wire := buildMessage(payload, chunkSize)
	cr := &ChunkedReader{r: bufio.NewReader(&cycleReader{buf: wire}), maxMessageBytes: DefaultMaxMessageBytes}
	var reuse []byte

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := cr.readMessageReuse(&reuse)
		if err != nil {
			b.Fatalf("readMessageReuse: %v", err)
		}
		if len(got) != payloadLen {
			b.Fatalf("len = %d, want %d", len(got), payloadLen)
		}
		sinkLen = len(got)
	}
}

var sinkLen int

func BenchmarkReadMessage_1chunk_C_reuseCEILING(b *testing.B) {
	benchReadMessageReuse(b, 512, 65535)
}
func BenchmarkReadMessage_4chunk_C_reuseCEILING(b *testing.B) {
	benchReadMessageReuse(b, 4*4096, 4096)
}
func BenchmarkReadMessage_16chunk_C_reuseCEILING(b *testing.B) {
	benchReadMessageReuse(b, 16*65535, 65535)
}

// ── Allocation regression gate ────────────────────────────────────────────
//
// ReadMessage must allocate exactly ONE heap object per single-chunk inbound
// message: the reassembled message buffer itself, which the caller owns and
// which therefore cannot be avoided without unsound buffer reuse (rmp #2716
// Part 2, refuted).
//
// It must NOT allocate a second object for the 2-byte chunk-length header.
// That object existed until #2716: a `var header [2]byte` local escaped to the
// heap because io.ReadFull takes an io.Reader INTERFACE ("moved to heap:
// header"), costing one object on EVERY inbound message. It is now the
// ChunkedReader.hdr field. This test fails if that local is ever reintroduced.
//
// Note the `make([]byte, chunkLen)` in the reassembly loop is NOT a second
// allocation: the compiler rewrites `append(s, make([]T, n)...)` via
// walk.extendSlice into growslice + memclr with no temporary. Replacing it
// with slices.Grow changes neither the object count nor the bytes.
func TestReadMessageAllocsPerMessage(t *testing.T) {
	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = byte(i%251) + 1
	}
	wire := buildMessage(payload, 65535)
	cr := &ChunkedReader{r: bufio.NewReader(&cycleReader{buf: wire}), maxMessageBytes: DefaultMaxMessageBytes}

	got := testing.AllocsPerRun(2000, func() {
		msg, err := cr.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		sinkMsg = msg
	})
	if got != wantMsgAllocs {
		t.Errorf("ReadMessage allocates %.0f objects per single-chunk message, want exactly %d "+
			"(the returned message buffer, plus the make temporary under -race). An extra "+
			"object means the chunk-length header escaped to the heap again — keep it in "+
			"ChunkedReader.hdr.", got, wantMsgAllocs)
	}
}
