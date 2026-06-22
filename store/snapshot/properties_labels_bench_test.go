package snapshot

import (
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildBenchLPG builds a deterministic labelled property graph reused across
// the WriteProperties / WriteLabels benchmarks: nodes carry three string
// properties and two labels each, and every edge carries one string property.
// The fixture is sized so the per-record encode cost dominates, which is what
// the allocation-regression gate watches: the record writers were de-reflected
// from per-field binary.Write (two heap allocations per field) to a single
// reused scratch buffer packed with PutUintNN, so allocs/op should be a small
// constant rather than scaling with the record count.
func buildBenchLPG(tb testing.TB) *lpg.Graph[int, int64] {
	tb.Helper()
	const nodes = 4000
	const k = 4 // out-degree
	g := lpg.New[int, int64](adjlist.Config{Directed: true})
	labels := [...]string{"Account", "Verified", "Premium", "Dormant"}
	for i := 0; i < nodes; i++ {
		for j := 1; j <= k; j++ {
			dst := (i + j) % nodes
			if err := g.AddEdge(i, dst, int64(i*k+j)); err != nil {
				tb.Fatalf("AddEdge(%d,%d): %v", i, dst, err)
			}
			if err := g.SetEdgeProperty(i, dst, "rel", lpg.StringValue("edge")); err != nil {
				tb.Fatalf("SetEdgeProperty(%d,%d): %v", i, dst, err)
			}
		}
		if err := g.SetNodeProperty(i, "name", lpg.StringValue("node")); err != nil {
			tb.Fatalf("SetNodeProperty name: %v", err)
		}
		if err := g.SetNodeProperty(i, "email", lpg.StringValue("user@example.com")); err != nil {
			tb.Fatalf("SetNodeProperty email: %v", err)
		}
		if err := g.SetNodeProperty(i, "score", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty score: %v", err)
		}
		if err := g.SetNodeLabel(i, labels[i%len(labels)]); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeLabel(i, "Account"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
	}
	return g
}

// BenchmarkWriteProperties measures the per-call allocation cost of
// serialising the property section. The record writers pack each fixed-width
// header into one reused scratch buffer and emit it in a single Write, so the
// transient allocation count no longer scales with the number of property
// records.
func BenchmarkWriteProperties(b *testing.B) {
	g := buildBenchLPG(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := WriteProperties(io.Discard, g); err != nil {
			b.Fatalf("WriteProperties: %v", err)
		}
	}
}

// BenchmarkWriteLabels measures the per-call allocation cost of serialising the
// label section under the same de-reflected record encoding.
func BenchmarkWriteLabels(b *testing.B) {
	g := buildBenchLPG(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := WriteLabels(io.Discard, g); err != nil {
			b.Fatalf("WriteLabels: %v", err)
		}
	}
}
