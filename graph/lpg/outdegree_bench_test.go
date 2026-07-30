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

// buildMixedTypeHub returns a graph of total nodes in which "hub" has degree
// hubDegree, its out-edges cycling through typeCount distinct relationship types
// so only a 1/typeCount fraction of the slots carry the counted type.
//
// This is the shape a typed degree must stay linear on. Any per-slot fallback
// that scans the pair's SIBLING slots to decide the slot's own type turns the
// walk quadratic here — an earlier attempt at rmp #2258 measured 12.80 µs →
// 2.34 ms on exactly this shape — while a uniformly-typed hub (buildHub) hides
// the regression entirely, because there every slot answers from its own column
// entry and no fallback is ever taken.
func buildMixedTypeHub(b *testing.B, total, hubDegree, typeCount int) *lpg.Graph[string, float64] {
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
		rel := fmt.Sprintf("T%d", i%typeCount)
		if err := g.AddEdgeLabeled("hub", fmt.Sprintf("n%d", i%total), 1, rel); err != nil {
			b.Fatalf("AddEdgeLabeled: %v", err)
		}
	}
	return g
}

// BenchmarkOutDegreeByType_MixedTypes is the typed degree over a hub whose slots
// carry FOUR different relationship types, so three quarters of the walk's slots
// do not carry the counted type and take whatever the handle-less fallback is.
// It must stay linear in the degree.
func BenchmarkOutDegreeByType_MixedTypes(b *testing.B) {
	const typeCount = 4
	for _, d := range []int{16, 256, 4096} {
		g := buildMixedTypeHub(b, 4096, d, typeCount)
		relType, ok := g.Registry().Lookup("T0")
		if !ok {
			b.Fatal("relationship type T0 was not interned")
		}
		want := (d + typeCount - 1) / typeCount
		b.Run(fmt.Sprintf("d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := g.OutDegreeByType("hub", relType); !okd || got != want {
					b.Fatalf("OutDegreeByType = (%d, %v), want (%d, true)", got, okd, want)
				}
			}
		})
	}
}

// BenchmarkOutDegreeByType_Parallel is the typed degree over a hub whose whole
// degree lands on ONE far node as parallel edges, half of them carrying the
// counted type. It is the multigraph shape rmp #2258 is about: a pair with many
// handle-less parallel slots, where a per-pair fallback both mis-counts and, if
// it scans siblings, degrades to O(d²).
func BenchmarkOutDegreeByType_Parallel(b *testing.B) {
	for _, d := range []int{16, 256, 4096} {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for _, k := range []string{"hub", "far"} {
			if err := g.AddNode(k); err != nil {
				b.Fatalf("AddNode: %v", err)
			}
		}
		for i := 0; i < d; i++ {
			rel := "T0"
			if i%2 == 1 {
				rel = "T1"
			}
			if err := g.AddEdgeLabeled("hub", "far", 1, rel); err != nil {
				b.Fatalf("AddEdgeLabeled: %v", err)
			}
		}
		relType, ok := g.Registry().Lookup("T0")
		if !ok {
			b.Fatal("relationship type T0 was not interned")
		}
		want := (d + 1) / 2
		b.Run(fmt.Sprintf("d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := g.OutDegreeByType("hub", relType); !okd || got != want {
					b.Fatalf("OutDegreeByType = (%d, %v), want (%d, true)", got, okd, want)
				}
			}
		})
	}
}

// BenchmarkOutDegreeByType_ByHandle is the typed degree over a hub built the way
// Cypher CREATE builds one: every slot carries a stable per-edge handle and its
// relationship type is recorded against that handle, exactly as
// cypher/exec.CreateRelationship writes it (AddEdgeH, then the per-pair
// SetEdgeLabel, then SetEdgeLabelByHandle).
//
// It has no meaningful "before": the column-only count this path replaced returned
// the WRONG answer on this shape (1 for three parallel :K edges, rmp #2241/#2258),
// so the number below is the price of the correct answer, not a regression against
// a comparable measurement. It is recorded so the cost of the handle-store probe —
// one map lookup under the pair's shard mutex per slot — is explicit rather than
// implied, and so a future change that hoists it has a baseline to beat.
func BenchmarkOutDegreeByType_ByHandle(b *testing.B) {
	for _, d := range []int{16, 256, 4096} {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		if err := g.AddNode("hub"); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		for i := 0; i < d; i++ {
			dst := fmt.Sprintf("n%d", i)
			if err := g.AddNode(dst); err != nil {
				b.Fatalf("AddNode: %v", err)
			}
			h, err := g.AddEdgeH("hub", dst, 1)
			if err != nil {
				b.Fatalf("AddEdgeH: %v", err)
			}
			g.SetEdgeLabel("hub", dst, "T")
			g.SetEdgeLabelByHandle("hub", dst, h, "T")
		}
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

// BenchmarkOutDegreeByTypeBounded_Shapes measures the BOUNDED walker, which is the
// form the Cypher engine actually calls (cypher/degree_rewrite.go and
// cypher/labelled_hop_count.go go through OutDegreeByTypeBoundedByID /
// OutDegreeMatchingBoundedByID). The unbounded lpg.Graph.OutDegreeByType has no
// production caller, so this is the measurement that decides whether a change to
// the shared walk costs a real query anything.
//
// A ceiling above the true count forces the walk to the end, which is the worst
// case and the one a `COUNT { … } = n` comparison produces.
func BenchmarkOutDegreeByTypeBounded_Shapes(b *testing.B) {
	for _, d := range []int{256, 4096} {
		// Column-typed hub: every slot's type is in the label column (no handles).
		g := buildMixedTypeHub(b, 4096, d, 4)
		relType, ok := g.Registry().Lookup("T0")
		if !ok {
			b.Fatal("relationship type T0 was not interned")
		}
		want := (d + 3) / 4
		b.Run(fmt.Sprintf("column/d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := g.OutDegreeByTypeBounded("hub", relType, d+1); !okd || got != want {
					b.Fatalf("OutDegreeByTypeBounded = (%d, %v), want (%d, true)", got, okd, want)
				}
			}
		})

		// Handle-bearing hub, built the way Cypher CREATE builds one.
		gh := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		if err := gh.AddNode("hub"); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		for i := 0; i < d; i++ {
			dst := fmt.Sprintf("n%d", i)
			if err := gh.AddNode(dst); err != nil {
				b.Fatalf("AddNode: %v", err)
			}
			h, err := gh.AddEdgeH("hub", dst, 1)
			if err != nil {
				b.Fatalf("AddEdgeH: %v", err)
			}
			rel := fmt.Sprintf("T%d", i%4)
			gh.SetEdgeLabel("hub", dst, rel)
			gh.SetEdgeLabelByHandle("hub", dst, h, rel)
		}
		relTypeH, okH := gh.Registry().Lookup("T0")
		if !okH {
			b.Fatal("relationship type T0 was not interned on the handle-bearing hub")
		}
		b.Run(fmt.Sprintf("byhandle/d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := gh.OutDegreeByTypeBounded("hub", relTypeH, d+1); !okd || got != want {
					b.Fatalf("OutDegreeByTypeBounded = (%d, %v), want (%d, true)", got, okd, want)
				}
			}
		})

		// A tight ceiling for a type NO slot carries: the walk cannot take the
		// specialised scan (the cap is below the degree) and cannot stop early
		// (nothing matches), so it runs the general per-slot resolver over every
		// slot. This is the worst case for the overflow gate the per-slot type
		// resolution added, and the shape a `COUNT { (a)-[:ABSENT]->() } > 0`
		// produces.
		absent := g.Registry().Intern("ABSENT")
		b.Run(fmt.Sprintf("absent-tight-cap/d=%d", d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, okd := g.OutDegreeByTypeBounded("hub", absent, 1); !okd || got != 0 {
					b.Fatalf("OutDegreeByTypeBounded = (%d, %v), want (0, true)", got, okd)
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
