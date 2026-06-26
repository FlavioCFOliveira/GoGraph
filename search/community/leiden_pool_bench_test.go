package community

// leiden_pool_bench_test.go — benchmark for #1725: cross-call pooling of the
// aggregate() level buffer-sets.
//
// Leiden's aggregate() builds, per pass, the persistent output arrays of one
// aggregation level (verts/edges/weights/deg/loop/lifted). Within a single
// Leiden call these are already recycled through a graphBufFreeList. But that
// free list lives only for the duration of one LeidenCtx call, so every fresh
// Leiden invocation starts with an empty free list and allocates the first
// level's arrays from scratch. A workload that runs Leiden repeatedly (a
// service answering many community-detection queries, the streaming re-run on
// an evolving graph, or simply the benchmark loop) pays that first-level
// allocation every call.
//
// This benchmark drives many Leiden calls over a moderate planted-partition
// graph — the repeated-call shape #1725 targets — and reports allocs/op + B/op
// + ns/op so a benchstat before/after comparison proves the cross-call pool
// removes per-call allocation without changing the result.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// BenchmarkLeiden_PlantedRepeated runs Leiden b.N times over a fixed
// planted-partition graph (k communities, dense intra-block, sparse inter-block
// edges). The repeated-call loop is the cross-call workload #1725 addresses: the
// before/after delta of allocs/op and B/op is the signal that the per-call
// first-level buffer allocation has been pooled away.
func BenchmarkLeiden_PlantedRepeated(b *testing.B) {
	const (
		k         = 16
		blockSize = 128
		pIn       = 30
		pOut      = 1
		seed      = 1234
	)
	g, err := shapegen.PlantedPartition(k, blockSize, pIn, pOut, seed).
		Build(adjlist.Config{Directed: false})
	if err != nil {
		b.Fatalf("PlantedPartition Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Leiden(c, DefaultLeidenOptions())
	}
}
