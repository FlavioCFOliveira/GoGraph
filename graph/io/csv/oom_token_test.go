package csv_test

import (
	"errors"
	"runtime"
	"testing"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// endlessQuotedField is an io.Reader that emits a single opening double
// quote followed by an unbounded run of 'x' bytes and never reaches EOF.
// Fed to encoding/csv it is an unterminated quoted field — the decoder
// buffers it until the byte cap trips, exercising the single-oversized-
// token amplification path (task #1436).
type endlessQuotedField struct{ started bool }

func (e *endlessQuotedField) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	i := 0
	if !e.started {
		p[0] = '"'
		e.started = true
		i = 1
	}
	for ; i < len(p); i++ {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestReadInto_SingleTokenBounded feeds an endless unterminated quoted
// field under a small cap and asserts (a) the read fails with the typed
// ErrInputTooLarge and (b) retained heap growth stays within a bounded
// ceiling — the decoder must not be able to drive RAM beyond a small
// multiple of the cap (mirrors the jsonl bounded-heap gate).
func TestReadInto_SingleTokenBounded(t *testing.T) {
	// Deliberately serial (no t.Parallel): the heap-growth assertion reads
	// process-global runtime.MemStats, so a concurrent allocator in the
	// package would add noise. The ErrInputTooLarge/nil-graph checks are
	// the deterministic gate; the heap bound is a wide-margin sanity check.
	const capBytes = 4 << 20 // 4 MiB explicit cap

	opts := csv.DefaultOptions()
	opts.MaxBytes = capBytes

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	a, n, err := csv.ReadInto(&endlessQuotedField{}, opts)
	if !errors.Is(err, csv.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 on cap error", n)
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	// A 4 MiB cap with ~4–5x decoder amplification peaks well under
	// 64 MiB; once the read fails the transient buffers are released, so
	// retained growth must be far below the ceiling.
	const maxHeapGrowth = 64 << 20 // 64 MiB
	if after.HeapAlloc > before.HeapAlloc+maxHeapGrowth {
		t.Errorf("heap grew by %d bytes (>%d); before=%d after=%d",
			after.HeapAlloc-before.HeapAlloc, maxHeapGrowth, before.HeapAlloc, after.HeapAlloc)
	}
}
