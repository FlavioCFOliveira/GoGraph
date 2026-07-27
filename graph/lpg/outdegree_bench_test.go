package lpg_test

// outdegree_bench_test.go — the degree primitive is O(1) (task #2218,
// acceptance criterion 5).
//
// Two sweeps, because "O(1)" has two independent claims to prove:
//
//   - BenchmarkOutDegree_ByNodeDegree grows the measured NODE's degree while the
//     graph stays the same size. Flat means the count is not an enumeration.
//   - BenchmarkOutDegree_ByGraphSize grows the GRAPH while the measured node's
//     degree stays fixed. Flat means nothing graph-wide is scanned.
//
// The enumeration baseline is included so the comparison is against the thing
// the primitive replaces, not against an abstract expectation.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkOutDegree' -benchmem ./graph/lpg/

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildHub returns a graph of total nodes in which "hub" has degree hubDegree.
func buildHub(b *testing.B, total, hubDegree int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("hub"); err != nil {
		b.Fatalf("AddNode: %v", err)
	}
	for i := 0; i < total; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < hubDegree; i++ {
		if err := g.AddEdgeLabeled("hub", fmt.Sprintf("n%d", i%total), 1, "T"); err != nil {
			b.Fatalf("AddEdgeLabeled: %v", err)
		}
	}
	return g
}

// enumerate counts by walking, which is what the primitive replaces.
func enumerate(g *lpg.Graph[string, float64], src string) int {
	n := 0
	for range g.AdjList().Neighbours(src) {
		n++
	}
	return n
}

// BenchmarkOutDegree_ByNodeDegree grows the measured node's degree. OutDegree
// must stay flat; the enumeration baseline must not.
func BenchmarkOutDegree_ByNodeDegree(b *testing.B) {
	for _, d := range []int{1, 16, 256, 4096} {
		g := buildHub(b, 4096, d)
		b.Run(fmt.Sprintf("OutDegree/d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, ok := g.OutDegree("hub"); !ok || got != d {
					b.Fatalf("OutDegree = (%d, %v), want (%d, true)", got, ok, d)
				}
			}
		})
		b.Run(fmt.Sprintf("enumerate/d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := enumerate(g, "hub"); got != d {
					b.Fatalf("enumerate = %d, want %d", got, d)
				}
			}
		})
	}
}

// BenchmarkOutDegree_ByGraphSize grows the graph while the measured node's degree
// is fixed, so any graph-wide work would show up here.
func BenchmarkOutDegree_ByGraphSize(b *testing.B) {
	const hubDegree = 8
	for _, total := range []int{1024, 16384, 262144} {
		g := buildHub(b, total, hubDegree)
		b.Run(fmt.Sprintf("total=%d", total), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, ok := g.OutDegree("hub"); !ok || got != hubDegree {
					b.Fatalf("OutDegree = (%d, %v), want (%d, true)", got, ok, hubDegree)
				}
			}
		})
	}
}

// BenchmarkOutDegreeByType is the typed variant, which is O(d) by construction
// because the relationship-type column must be read. It is measured so the
// difference from the O(1) untyped path is on the record rather than implied.
func BenchmarkOutDegreeByType(b *testing.B) {
	for _, d := range []int{1, 16, 256, 4096} {
		g := buildHub(b, 4096, d)
		relType, ok := g.Registry().Lookup("T")
		if !ok {
			b.Fatal("relationship type T was not interned")
		}
		b.Run(fmt.Sprintf("d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := g.OutDegreeByType("hub", relType); !okd || got != d {
					b.Fatalf("OutDegreeByType = (%d, %v), want (%d, true)", got, okd, d)
				}
			}
		})
	}
}

// BenchmarkOutDegree_WithTombstones measures the filtered path, taken once the
// graph holds any tombstone. It is O(d), and the point of measuring it is to show
// what the fast path buys.
func BenchmarkOutDegree_WithTombstones(b *testing.B) {
	const d = 256
	g := buildHub(b, 4096, d)
	g.RemoveNode("n4000") // one tombstone is enough to leave the O(1) path
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := g.OutDegree("hub"); !ok {
			b.Fatal("OutDegree reported not-interned")
		}
	}
}
