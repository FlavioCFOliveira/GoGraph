package search

import (
	"errors"
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBellmanFord_HandBuilt(t *testing.T) {
	t.Parallel()
	edges := []weightedEdge{
		{0, 1, 6}, {0, 2, 7},
		{1, 2, 8}, {1, 3, 5}, {1, 4, -4},
		{2, 3, -3}, {2, 4, 9},
		{3, 1, -2},
		{4, 0, 2}, {4, 3, 7},
	}
	c, a := buildWeightedCSR(edges)
	src, _ := a.Mapper().Lookup(0)
	d, err := BellmanFord(c, src)
	if err != nil {
		t.Fatalf("BellmanFord: %v", err)
	}
	// Distances from 0 in this CLRS example (Section 24.1, Fig. 24.4).
	want := map[int]int64{0: 0, 1: 2, 2: 7, 3: 4, 4: -2}
	for k, expected := range want {
		nodeID, _ := a.Mapper().Lookup(k)
		got, ok := d.Distance(nodeID)
		if !ok {
			t.Fatalf("node %d not reachable", k)
		}
		if got != expected {
			t.Fatalf("Distance(%d) = %d, want %d", k, got, expected)
		}
	}
}

func TestBellmanFord_DetectNegativeCycle(t *testing.T) {
	t.Parallel()
	// Cycle 0 -> 1 -> 2 -> 0 with total weight -1.
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 1}, {1, 2, -3}, {2, 0, 1},
	})
	src, _ := a.Mapper().Lookup(0)
	_, err := BellmanFord(c, src)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("expected ErrNegativeCycle, got %v", err)
	}
}

func TestBellmanFord_NegativeWeightsNoCycle(t *testing.T) {
	t.Parallel()
	// 0 --(-1)--> 1 --2--> 2
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, -1}, {1, 2, 2}})
	src, _ := a.Mapper().Lookup(0)
	d, err := BellmanFord(c, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	id1, _ := a.Mapper().Lookup(1)
	id2, _ := a.Mapper().Lookup(2)
	if got, _ := d.Distance(id1); got != -1 {
		t.Fatalf("Distance(1) = %d, want -1", got)
	}
	if got, _ := d.Distance(id2); got != 1 {
		t.Fatalf("Distance(2) = %d, want 1", got)
	}
}

// TestBellmanFord_RandomisedAgainstDijkstra verifies parity with
// Dijkstra over random non-negative graphs.
func TestBellmanFord_RandomisedAgainstDijkstra(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewPCG(seed, 23)) //nolint:gosec // deterministic test RNG
		const n = 64
		const e = 256
		edges := make([]weightedEdge, 0, e)
		for i := 0; i < e; i++ {
			edges = append(edges, weightedEdge{r.IntN(n), r.IntN(n), int64(r.IntN(50) + 1)})
		}
		c, a := buildWeightedCSRCfg(edges, adjlist.Config{Directed: true, Multigraph: true})
		src := r.IntN(n)
		srcID, _ := a.Mapper().Lookup(src)
		gotBF, err := BellmanFord(c, srcID)
		if err != nil {
			t.Fatalf("seed=%d: BF: %v", seed, err)
		}
		gotDij, err := Dijkstra(c, srcID)
		if err != nil {
			t.Fatalf("seed=%d: Dij: %v", seed, err)
		}
		for v := 0; v < n; v++ {
			id, ok := a.Mapper().Lookup(v)
			if !ok {
				continue
			}
			db, okb := gotBF.Distance(id)
			dd, okd := gotDij.Distance(id)
			if okb != okd {
				t.Fatalf("seed=%d node %d reachability: BF=%v Dij=%v", seed, v, okb, okd)
			}
			if okb && db != dd {
				t.Fatalf("seed=%d node %d: BF=%d Dij=%d", seed, v, db, dd)
			}
		}
	}
}

func BenchmarkBellmanFord_10kVertices(b *testing.B) {
	a := adjlist.New[uint32, int64](adjlist.Config{Directed: true})
	const universe = 1 << 14 // 16384 nodes
	for i := uint32(0); i < uint32(universe); i++ {
		a.AddNode(i)
	}
	r := rand.New(rand.NewPCG(31, 1)) //nolint:gosec // deterministic benchmark RNG
	const fill = universe * 4
	for i := 0; i < fill; i++ {
		a.AddEdge(uint32(r.IntN(universe)), uint32(r.IntN(universe)), int64(r.IntN(100)+1))
	}
	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(uint32(0))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = BellmanFord(c, srcID)
	}
}
