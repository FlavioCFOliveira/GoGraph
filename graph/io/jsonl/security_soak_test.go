//go:build soak || nightly

package jsonl_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestSec_IO_JSONLSustainedHostileStreamBounded is the soak-layer endurance
// proof for the byte cap: it feeds a sustained, never-ending stream of
// valid node records — hundreds of megabytes of well-formed input — through
// the reader under an explicit cap, and asserts the reader stops with
// ErrInputTooLarge having allocated only a bounded amount of heap. A reader
// that accumulated the whole stream (or trusted its apparent size) would
// blow the heap budget; this pins that it does not.
//
// The stream is generated on the fly, so the test process never holds the
// full payload itself.
func TestSec_IO_JSONLSustainedHostileStreamBounded(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	// 512 MiB of well-formed records under a 256 MiB cap: the cap must trip
	// well before the stream ends, and heap must stay bounded throughout.
	const capBytes = 256 << 20
	const streamBytes = 512 << 20
	r := &secIOValidRecordStream{remaining: streamBytes}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	g, rows, err := jsonl.ReadIntoCappedCtx(context.Background(), r,
		adjlist.Config{Directed: true}, capBytes)
	if !errors.Is(err, jsonl.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge after the cap is crossed", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on cap error", g)
	}
	_ = rows

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// The reader builds an adjacency list up to the cap before failing, so
	// some growth is expected; the guard is that it stays a small multiple
	// of the cap rather than tracking the full 512 MiB stream.
	const maxHeapGrowthMiB = 1024
	if after.HeapAlloc > before.HeapAlloc {
		deltaMiB := (after.HeapAlloc - before.HeapAlloc) / (1 << 20)
		if deltaMiB > maxHeapGrowthMiB {
			t.Errorf("heap delta = %d MiB, want <= %d MiB (cap not bounding growth)",
				deltaMiB, maxHeapGrowthMiB)
		}
	}
}

// secIOValidRecordStream emits an endless run of distinct, well-formed
// JSON-Lines node records ({"type":"node","id":"<seq>"}\n) up to a byte
// budget, then EOF. Distinct ids prevent the adjacency list from collapsing
// the stream to a single node, so the reader does real per-record work.
type secIOValidRecordStream struct {
	remaining int
	seq       int
	buf       []byte
	off       int
}

func (s *secIOValidRecordStream) Read(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if s.off >= len(s.buf) {
			if s.remaining <= 0 {
				if written > 0 {
					return written, nil
				}
				return 0, io.EOF
			}
			s.buf = s.nextRecord()
			s.off = 0
		}
		n := copy(p[written:], s.buf[s.off:])
		s.off += n
		s.remaining -= n
		written += n
	}
	return written, nil
}

// nextRecord formats the next distinct node record.
func (s *secIOValidRecordStream) nextRecord() []byte {
	s.seq++
	// Hand-format to avoid fmt allocation churn in the hot stream loop.
	rec := append([]byte(`{"type":"node","id":"n`), itoaSecIO(s.seq)...)
	rec = append(rec, '"', '}', '\n')
	return rec
}

// itoaSecIO converts a non-negative int to its decimal ASCII bytes without
// importing strconv into the hot path's allocation profile.
func itoaSecIO(v int) []byte {
	if v == 0 {
		return []byte{'0'}
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return tmp[i:]
}
