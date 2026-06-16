package centrality

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestBetweenness_Path(t *testing.T) {
	t.Parallel()
	// Undirected path 0-1-2-3-4. Centre node 2 has the highest
	// betweenness; nodes 0/4 are zero.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	id4, _ := a.Mapper().Lookup(4)
	if bc[uint64(id0)] != 0 || bc[uint64(id4)] != 0 {
		t.Fatalf("endpoint betweenness must be 0")
	}
	if bc[uint64(id2)] <= bc[uint64(id0)] {
		t.Fatalf("centre betweenness must exceed endpoints")
	}
}

// BenchmarkBrandes_RandomGraph measures the steady-state allocation
// profile of Betweenness on a moderate undirected random graph. The
// brandesSource per-source loop is the hot path; task #126 requires
// allocs/op to drop by >=50% versus the v1.0 implementation, achieved
// by lifting the queue and stack allocations to the outer loop.
func BenchmarkBrandes_RandomGraph(b *testing.B) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 512
	r := rand.New(rand.NewPCG(19, 23)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 3*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Betweenness(c)
	}
}

// buildRandomCSR builds an undirected random graph of n vertices with
// avgDeg*n undirected edges (so ~avgDeg/2 expected degree per vertex
// after de-duplication by the adjacency list). The PCG seed is fixed
// so every run is bit-for-bit reproducible. It is the shared fixture
// for the scale benchmarks below.
func buildRandomCSR(b *testing.B, n, avgDeg int) *csr.CSR[struct{}] {
	b.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(19, 23)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < avgDeg*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// BenchmarkBrandes_Scale measures Betweenness across larger and denser
// undirected random graphs than [BenchmarkBrandes_RandomGraph]. The
// flat predecessor arena (#1515) trades one extra O(E) in-degree pass
// for a contiguous, pointer-chase-free accumulation walk; that
// cache-locality win only manifests once the predecessor working set
// exceeds the L1/L2 footprint, which the 512-node guard-band graph is
// far too small to reach. These sizes make the locality effect — and
// the near-zero steady-state allocation — measurable, per the
// "measure to decide" mandate. Brandes is O(V*E), so the larger sizes
// are intentionally capped; they remain comfortably inside the short
// test layer's per-package budget under -benchtime.
func BenchmarkBrandes_Scale(b *testing.B) {
	cases := []struct {
		name   string
		n      int
		avgDeg int
	}{
		{"2k_deg4", 2000, 4},
		{"2k_deg16", 2000, 16},
		{"5k_deg4", 5000, 4},
		{"5k_deg16", 5000, 16},
		{"10k_deg4", 10000, 4},
	}
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			c := buildRandomCSR(b, tc.n, tc.avgDeg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Betweenness(c)
			}
		})
	}
}

func TestBetweenness_Star(t *testing.T) {
	t.Parallel()
	// Star: hub 0 connected to 1..4. Hub has max betweenness;
	// every leaf has 0.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 1; i <= 4; i++ {
		if err := a.AddEdge(0, i, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	hub, _ := a.Mapper().Lookup(0)
	if math.Abs(bc[uint64(hub)]-12) > 1e-9 { // 4*3 ordered pairs through hub
		t.Fatalf("hub betweenness = %f, want 12", bc[uint64(hub)])
	}
}
