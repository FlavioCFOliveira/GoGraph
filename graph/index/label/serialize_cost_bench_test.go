package label

import (
	"io"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// serialize_cost_bench_test.go — the cost bound #2609's design rests on.
//
// Serialize normalises a label's container encoding so that a Serialize/
// Deserialize cycle reproduces its own bytes. The normalisation is BOUNDED at
// smallSetMax ids because normalising an arbitrary set requires cloning it
// first: Serialize holds only a read lock and NodeSet.Bitmap hands back the live
// bitmap, while roaring's run optimisation rewrites containers in place.
//
// Measured, an unbounded normalisation cost a sparse 100 000-id label 6.55 to
// 90 microseconds and 1 289 to 218 065 bytes per serialize, to produce a
// BYTE-IDENTICAL image. The bounded normalisation costs those shapes nothing:
// interleaved A/B over five pairs found allocations IDENTICAL at 15/op for both
// the dense and the sparse label, and identical at 26/op for an Add-built label
// of 8 ids, with every sample equal.
//
// These benchmarks make that visible so a later change that removes the bound
// cannot do so unnoticed: allocations per op must stay flat as cardinality
// grows.

func benchLabel(b *testing.B, build func(*Index)) {
	idx := NewIndex()
	build(idx)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Serialize(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSerializeDenseContiguous is the shape the normalisation could have
// been most costly on: 100 000 contiguous ids added one at a time, so the set is
// far above the bound and holds bitmap containers.
func BenchmarkSerializeDenseContiguous(b *testing.B) {
	benchLabel(b, func(i *Index) {
		for k := 0; k < 100_000; k++ {
			i.Add(1, graph.NodeID(1000+k))
		}
	})
}

// BenchmarkSerializeSparse is the shape an unbounded normalisation wasted the
// most work on: run encoding cannot shrink it, so the clone bought nothing.
func BenchmarkSerializeSparse(b *testing.B) {
	benchLabel(b, func(i *Index) {
		r := rand.New(rand.NewPCG(1, 2))
		for k := 0; k < 100_000; k++ {
			i.Add(1, graph.NodeID(r.Uint64N(10_000_000)))
		}
	})
}

// BenchmarkSerializeWithinTheBound is the band that IS normalised, so the cost
// of the repair itself is visible rather than inferred.
func BenchmarkSerializeWithinTheBound(b *testing.B) {
	benchLabel(b, func(i *Index) { i.AddRange(1, 1000, 1007) })
}

// BenchmarkSerializeSmallAddBuilt is the common small label: at or below the
// bound but built by Add, so it is on an inline tier. NodeSet.Bitmap already
// materialises it from the sorted ids, which is the canonical form, so the
// normalisation must cost it nothing at all.
func BenchmarkSerializeSmallAddBuilt(b *testing.B) {
	benchLabel(b, func(i *Index) {
		for k := 0; k < 8; k++ {
			i.Add(1, graph.NodeID(1000+k))
		}
	})
}
