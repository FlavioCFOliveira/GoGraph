package csv

import (
	"fmt"
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkWriteCSV drives the CSV edge-list writer over a weighted ring. The
// 3-cell record slice is now reused across edges rather than allocated per
// edge; the weight string remains the encoding/csv []string floor.
func BenchmarkWriteCSV(b *testing.B) {
	const n = 200_000
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := a.AddEdge(fmt.Sprintf("n%07d", i), fmt.Sprintf("n%07d", (i+1)%n), int64(i%97+1)); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Write(io.Discard, a, Options{}); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}

// BenchmarkWriteCSV_LargeWeights drives the writer with weights that
// exceed strconv's cached small-int range (|w| >= 100), where the old
// per-edge strconv.FormatInt heap-allocated one string per edge. The
// AppendInt-into-reused-scratch path (rmp #1523) removes that allocation,
// so this benchmark's allocs/op must stay flat regardless of n.
func BenchmarkWriteCSV_LargeWeights(b *testing.B) {
	const n = 200_000
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		// Weights well above the 0..99 cache so FormatInt would allocate.
		w := int64(100_000 + i*7)
		if err := a.AddEdge(fmt.Sprintf("n%07d", i), fmt.Sprintf("n%07d", (i+1)%n), w); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Write(io.Discard, a, Options{}); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}
