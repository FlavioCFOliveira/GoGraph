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
