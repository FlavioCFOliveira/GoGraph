package dot

import (
	"fmt"
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkWriteDOT drives the DOT writer over a weighted ring so every edge
// carries a non-zero weight label. The edge statement is now assembled in a
// reused per-edge buffer with strconv.AppendInt for the weight, so the integer
// (weight) path allocates nothing — only the node-name quoting remains on the
// string path.
func BenchmarkWriteDOT(b *testing.B) {
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
		if err := Write(io.Discard, a); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}
