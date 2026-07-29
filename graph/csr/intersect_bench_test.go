package csr

// intersect_bench_test.go — permanent benchmarks for the multi-way sorted-set
// intersection primitive (rmp #2156).
//
// Layer: short.
//
//	go test -run='^$' -bench=BenchmarkIntersect -benchmem -count=6 ./graph/csr/
//
// The claim these substantiate is that the primitive's cost tracks the SMALLEST
// participating set rather than the total input. That is the property a plain
// sorted merge does not have, and without it the primitive has no reason to exist:
// summed over a triangle's driving edges, a merge's Σ(p+q) is identically the
// two-path count the binary-join plan already pays.
//
// Read BenchmarkIntersect_SmallVsLarge across its sizes rather than as points. The
// large side grows 10× per step while the small side stays fixed, so a merge-based
// implementation would grow roughly linearly with it and a galloping one should be
// close to flat. TestIntersector_CostTracksSmallestSet asserts the same property
// deterministically by counting slot visits — the benchmark shows the shape, the
// test is the gate, because this project has repeatedly been misled by timings.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ascending builds an ascending run of n destinations with the given stride, so two
// runs can be made to overlap in a controlled fraction of their slots.
func ascending(n, stride int) Range {
	e := make([]graph.NodeID, n)
	for i := range e {
		e[i] = graph.NodeID(i * stride)
	}
	return Range{Edges: e, Start: 0, End: uint64(n)}
}

// benchDrain runs one Init+drain and returns the number of results, keeping the
// compiler from eliminating the work.
func benchDrain(it *Intersector, ranges []Range) int {
	if !it.Init(ranges) {
		return 0
	}
	n := 0
	for {
		if _, ok := it.Next(); !ok {
			return n
		}
		n++
	}
}

// BenchmarkIntersect_SmallVsLarge holds the small side at 8 slots while the large
// side grows 10× per step. Near-flat ns/op is the galloping signature; growth
// proportional to the large side is the merge signature.
//
// Compare n=10k against n=100k, NOT n=1k against n=10k. The small side is strided
// by 1000, so at n=1k only one of its eight elements is inside the large range and
// at n=10k all eight are: the 1k→10k step therefore reflects the RESULT count
// rising from 1 to 8, not the input size. From 10k to 100k the result count is
// fixed at 8 and the input grows 10×, which is the clean comparison — measured
// 95.14 ns/op against 96.57 ns/op.
func BenchmarkIntersect_SmallVsLarge(b *testing.B) {
	small := ascending(8, 1000)
	for _, large := range []int{1000, 10000, 100000} {
		bigRange := ascending(large, 1)
		ranges := []Range{small, bigRange}
		var it Intersector
		benchDrain(&it, ranges) // warm
		b.Run(sizeName(large), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = benchDrain(&it, ranges)
			}
		})
	}
}

// BenchmarkIntersect_EqualSizes is the case with no size asymmetry to exploit — the
// honest worst case for galloping, where it degenerates towards a merge. Recorded so
// the primitive's floor is on the record alongside its best case.
func BenchmarkIntersect_EqualSizes(b *testing.B) {
	for _, n := range []int{64, 1024, 16384} {
		a := ascending(n, 2) // evens
		c := ascending(n, 2) // evens: full overlap
		ranges := []Range{a, c}
		var it Intersector
		benchDrain(&it, ranges)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = benchDrain(&it, ranges)
			}
		})
	}
}

// BenchmarkIntersect_Disjoint measures the empty-result path, which the cyclic
// operator hits on every driving edge whose intersection closes no cycle. It must
// not cost more than finding matches would.
func BenchmarkIntersect_Disjoint(b *testing.B) {
	odds := ascending(4096, 2)
	evens := Range{Edges: shiftBy(odds.Edges, 1), Start: 0, End: odds.End}
	ranges := []Range{odds, evens}
	var it Intersector
	benchDrain(&it, ranges)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = benchDrain(&it, ranges)
	}
}

// BenchmarkIntersect_ThreeWay covers a chorded pattern, where a variable has three
// already-bound neighbours and the intersection is genuinely multi-way.
func BenchmarkIntersect_ThreeWay(b *testing.B) {
	a := ascending(2048, 2)
	c := ascending(2048, 3)
	d := ascending(2048, 5)
	ranges := []Range{a, c, d}
	var it Intersector
	benchDrain(&it, ranges)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = benchDrain(&it, ranges)
	}
}

// shiftBy returns a copy of e with every element increased by delta.
func shiftBy(e []graph.NodeID, delta int) []graph.NodeID {
	out := make([]graph.NodeID, len(e))
	for i, v := range e {
		out[i] = v + graph.NodeID(delta)
	}
	return out
}

// sizeName renders a benchmark sub-name without importing strconv at call sites.
func sizeName(n int) string {
	switch {
	case n >= 1000000:
		return "n=" + itoaBench(n/1000000) + "M"
	case n >= 1000:
		return "n=" + itoaBench(n/1000) + "k"
	default:
		return "n=" + itoaBench(n)
	}
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// sink prevents the drain from being optimised away.
var sink int
