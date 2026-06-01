package community

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// BenchmarkLeiden_RandomGraph measures the steady-state allocation
// profile of Leiden on a moderate undirected random graph. Task #127
// requires allocs/op to drop by >95% and ns/op by >5x versus the v1.0
// per-vertex map[int]float64 implementation, achieved by replacing
// the per-vertex map with a preallocated scratch+touched-list combo.
//
// The graph size (n=1e5) is the acceptance-bench size specified by
// task #127; at this scale the per-vertex map allocation cost
// dominates total wall-clock time and the win is most visible.
func BenchmarkLeiden_RandomGraph(b *testing.B) {
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	const n = 100_000
	r := rand.New(rand.NewPCG(29, 31)) //nolint:gosec // deterministic benchmark RNG
	// Plant two clusters with denser intra-cluster edges so Leiden has
	// real structure to find.
	for i := 0; i < 8*n; i++ {
		var from, to int
		if r.IntN(4) < 3 {
			cluster := r.IntN(2)
			from = cluster*n/2 + r.IntN(n/2)
			to = cluster*n/2 + r.IntN(n/2)
		} else {
			from = r.IntN(n)
			to = r.IntN(n)
		}
		if err := a.AddEdge(from, to, 1.0); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Leiden(c, DefaultLeidenOptions())
	}
}

// BenchmarkLabelPropagation_RandomGraph mirrors BenchmarkLeiden but
// for the simpler Raghavan-Albert-Kumara label-propagation algorithm.
// The per-vertex map[int]int allocation was the v1.0 hot-path
// bottleneck.
func BenchmarkLabelPropagation_RandomGraph(b *testing.B) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 4096
	r := rand.New(rand.NewPCG(41, 43)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 8*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = LabelPropagation(c, DefaultLabelPropagationOptions())
	}
}

func TestLeiden_TwoCliques(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Two K4 cliques joined by a single bridge.
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	if err := a.AddEdge(3, 4, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	// Traag-Waltman Leiden separates two strongly-internally-connected
	// cliques across a single bridge edge: modularity gain on moving
	// any K4 node to the opposite clique is negative.
	if p.NumCommunities != 2 {
		t.Fatalf("Leiden TwoCliques found %d communities, want 2", p.NumCommunities)
	}
	// Verify the partition correctly groups the two K4s.
	mask := c.LiveMask()
	for id, m := range mask {
		if m {
			if p.Community[id] < 0 {
				t.Fatalf("live NodeID %d got sentinel community", id)
			}
		} else if p.Community[id] != -1 {
			t.Fatalf("ghost NodeID %d got community %d, want -1", id, p.Community[id])
		}
	}
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	id4, _ := a.Mapper().Lookup(4)
	id7, _ := a.Mapper().Lookup(7)
	if p.Community[id0] != p.Community[id3] {
		t.Fatalf("left clique split: c(0)=%d c(3)=%d", p.Community[id0], p.Community[id3])
	}
	if p.Community[id4] != p.Community[id7] {
		t.Fatalf("right clique split: c(4)=%d c(7)=%d", p.Community[id4], p.Community[id7])
	}
	if p.Community[id0] == p.Community[id4] {
		t.Fatalf("Leiden merged left and right cliques: c(0)=c(4)=%d", p.Community[id0])
	}
}

// TestLeiden_DisconnectedComponents stresses the post-pass that
// splits any disconnected community into its connected components —
// the Leiden-vs-Louvain guarantee that the v1 simplification still
// keeps.
func TestLeiden_DisconnectedComponents(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Two fully-disjoint K3 cliques.
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	for i := 3; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	if p.NumCommunities != 2 {
		t.Fatalf("Leiden on disjoint K3+K3 found %d communities, want 2", p.NumCommunities)
	}
}

func TestLabelPropagation_TwoCliques(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	if err := a.AddEdge(3, 4, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	p := LabelPropagation(c, DefaultLabelPropagationOptions())
	if p.NumCommunities < 1 {
		t.Fatalf("LabelPropagation found 0 communities")
	}
}

func TestLeiden_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	if p.NumCommunities != 0 {
		t.Fatalf("empty: %+v", p)
	}
}
