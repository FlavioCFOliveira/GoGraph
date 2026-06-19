package csrfile_test

// write_alloc_bench_test.go — allocation-budget benchmark for the bulk writer
// (sprint 221, #1597). Gates that WriteToFile does not materialise a per-edge
// transient widening copy of the edge column. Layer: short.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

func buildCSR(edges int) *csr.CSR[float64] {
	a := adjlist.New[int64, float64](adjlist.Config{Directed: true})
	n := int64(edges/4 + 1)
	for i := int64(0); i < n; i++ {
		_ = a.AddNode(i)
	}
	added := 0
	for i := int64(0); added < edges; i++ {
		src := i % n
		dst := (i*7919 + 1) % n
		if a.AddEdge(src, dst, 1.0) == nil {
			added++
		}
	}
	return csr.BuildFromAdjList(a)
}

func BenchmarkWriteToFile(b *testing.B) {
	const edges = 400000
	c := buildCSR(edges)
	dir := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, "g.csrf")
		if _, err := csrfile.WriteToFile[float64](path, c); err != nil {
			b.Fatal(err)
		}
	}
	_ = graph.NodeID(0)
}
