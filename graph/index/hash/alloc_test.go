package hash

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestHotPathsAreAllocationFree pins the allocation behaviour of the read and
// steady-state write paths the per-entry geometry of rmp #2692 exists to serve.
//
// The module's ULTRA EFFICIENT mandate requires efficiency to be MEASURED rather
// than claimed, and this geometry added a second lock level to paths that are
// taken millions of times per query. An accidental heap allocation there — a
// closure that escapes, a boxed conversion, a returned pointer that outlives its
// frame — would be invisible to every other test in this package and would cost
// far more than the lock traffic the refactor removed.
//
// It is deliberately NOT a t.Parallel() test. testing.AllocsPerRun reads the
// process-global malloc counter, so another test allocating concurrently would
// inflate the reading; Go parks parallel tests until every sequential test has
// finished, so a sequential test has the process to itself.
func TestHotPathsAreAllocationFree(t *testing.T) {
	idx := New[int64]()
	for v := int64(0); v < 1000; v++ {
		// Two nodes per key, so the key sits on the inline small-set tier and a
		// further Insert of an id already present is the steady-state write.
		idx.Insert(v, graph.NodeID(uint64(v)))
		idx.Insert(v, graph.NodeID(uint64(v)+100_000))
	}

	buf := make([]uint64, 0, 16)
	cases := []struct {
		name string
		fn   func()
	}{
		{"Cardinality (hit)", func() { _ = idx.Cardinality(500) }},
		{"Cardinality (miss)", func() { _ = idx.Cardinality(-1) }},
		{"Contains (hit)", func() { _ = idx.Contains(500, graph.NodeID(500)) }},
		{"Contains (miss)", func() { _ = idx.Contains(-1, graph.NodeID(500)) }},
		{"LookupAppend into a reused buffer", func() { _ = idx.LookupAppend(500, buf[:0]) }},
		{"DistinctValues", func() { _ = idx.DistinctValues() }},
		{"Insert of an id already present", func() { idx.Insert(500, graph.NodeID(500)) }},
		{"Delete of an absent id", func() { idx.Delete(500, graph.NodeID(7_000_000)) }},
	}
	for _, c := range cases {
		if n := testing.AllocsPerRun(200, c.fn); n != 0 {
			t.Errorf("%s: %v allocs/op, want 0", c.name, n)
		}
	}
}
