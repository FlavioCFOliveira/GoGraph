package graphml_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// endlessReader emits prefix once, then an unbounded run of fill bytes,
// and never reaches EOF.
type endlessReader struct {
	prefix []byte
	fill   byte
	pos    int
}

func (e *endlessReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && e.pos < len(e.prefix) {
		p[n] = e.prefix[e.pos]
		e.pos++
		n++
	}
	for ; n < len(p); n++ {
		p[n] = e.fill
	}
	return n, nil
}

// TestReadIntoCapped_SingleTokenBounded feeds an unterminated XML
// attribute value (a single oversized token) under a small cap and
// asserts (a) the read fails with the typed ErrInputTooLarge and (b)
// retained heap growth stays within a bounded ceiling. encoding/xml does
// not cap a single token, so the only guard is the byte cap; this pins
// that the cap keeps allocation to a small multiple of itself (#1436).
func TestReadIntoCapped_SingleTokenBounded(t *testing.T) {
	// Deliberately serial (no t.Parallel): the heap-growth assertion reads
	// process-global runtime.MemStats. The ErrInputTooLarge/nil-graph
	// checks are the deterministic gate; the heap bound is a sanity check.
	const capBytes = 4 << 20 // 4 MiB explicit cap

	// "<graphml><graph><node id="xxxx… — the attribute value never closes.
	r := &endlessReader{prefix: []byte(`<graphml><graph><node id="`), fill: 'x'}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	a, n, err := graphml.ReadIntoCappedCtx(context.Background(), r, capBytes)
	if !errors.Is(err, graphml.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
	_ = n

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	const maxHeapGrowth = 64 << 20 // 64 MiB
	if after.HeapAlloc > before.HeapAlloc+maxHeapGrowth {
		t.Errorf("heap grew by %d bytes (>%d); before=%d after=%d",
			after.HeapAlloc-before.HeapAlloc, maxHeapGrowth, before.HeapAlloc, after.HeapAlloc)
	}
}
