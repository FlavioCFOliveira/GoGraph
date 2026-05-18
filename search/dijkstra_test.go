package search

import (
	"errors"
	"math/rand/v2"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func buildWeightedCSR(edges []weightedEdge) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	return buildWeightedCSRCfg(edges, adjlist.Config{Directed: true})
}

func buildWeightedCSRCfg(edges []weightedEdge, cfg adjlist.Config) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	a := adjlist.New[int, int64](cfg)
	for _, e := range edges {
		a.AddEdge(e.from, e.to, e.w)
	}
	return csr.BuildFromAdjList(a), a
}

type weightedEdge struct {
	from, to int
	w        int64
}

func TestDijkstra_HandBuilt(t *testing.T) {
	t.Parallel()
	// CLRS-style graph:
	//   0 --10--> 1
	//   0 --3--> 2
	//   1 --1--> 3
	//   2 --4--> 1
	//   2 --8--> 3
	//   3 --7--> 4
	//   2 --2--> 4
	edges := []weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	}
	c, a := buildWeightedCSR(edges)
	src, _ := a.Mapper().Lookup(0)
	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}
	want := map[int]int64{0: 0, 1: 7, 2: 3, 3: 8, 4: 5}
	for k, expected := range want {
		nodeID, _ := a.Mapper().Lookup(k)
		got, ok := d.Distance(nodeID)
		if !ok {
			t.Fatalf("Distance(%d): not reachable", k)
		}
		if got != expected {
			t.Fatalf("Distance(%d) = %d, want %d", k, got, expected)
		}
	}
}

func TestDijkstra_Unreachable(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 5}, {1, 2, 6}, {3, 4, 1}})
	src, _ := a.Mapper().Lookup(0)
	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// 3 and 4 are unreachable.
	for _, k := range []int{3, 4} {
		nodeID, _ := a.Mapper().Lookup(k)
		if _, ok := d.Distance(nodeID); ok {
			t.Fatalf("node %d should be unreachable from 0", k)
		}
	}
}

func TestDijkstra_NegativeWeight(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{{0, 1, -3}, {1, 2, 4}})
	_, err := Dijkstra(c, graph.NodeID(0))
	if !errors.Is(err, ErrNegativeWeight) {
		t.Fatalf("expected ErrNegativeWeight, got %v", err)
	}
}

func TestDijkstra_PathReconstruction(t *testing.T) {
	t.Parallel()
	// Two paths from 0 to 3: 0->1->3 cost 4, 0->2->3 cost 3 (winner).
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	path := d.Path(dst)
	if len(path) != 3 {
		t.Fatalf("path length = %d, want 3", len(path))
	}
	mapper := a.Mapper()
	expected := []int{0, 2, 3}
	for i, id := range path {
		v, _ := mapper.Resolve(id)
		if v != expected[i] {
			t.Fatalf("path[%d] = %d, want %d", i, v, expected[i])
		}
	}
}

func TestDijkstra_Source(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 5}})
	src, _ := a.Mapper().Lookup(0)
	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.Source() != src {
		t.Fatalf("Source = %d, want %d", d.Source(), src)
	}
	dist, ok := d.Distance(src)
	if !ok || dist != 0 {
		t.Fatalf("Distance(src) = (%d, %v), want (0, true)", dist, ok)
	}
	path := d.Path(src)
	if len(path) != 1 || path[0] != src {
		t.Fatalf("Path(src) = %v, want [%d]", path, src)
	}
}

// TestDijkstra_RandomisedAgainstNaive verifies Dijkstra's result
// against a naive Bellman-Ford-style reference computed independently.
// Uses non-negative integer weights to keep both implementations
// applicable.
func TestDijkstra_RandomisedAgainstNaive(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewPCG(seed, 17)) //nolint:gosec // deterministic test RNG
		const n = 64
		const e = 256
		edges := make([]weightedEdge, 0, e)
		for i := 0; i < e; i++ {
			edges = append(edges, weightedEdge{
				from: r.IntN(n),
				to:   r.IntN(n),
				w:    int64(r.IntN(50) + 1),
			})
		}
		c, a := buildWeightedCSRCfg(edges, adjlist.Config{Directed: true, Multigraph: true})
		src := r.IntN(n)
		srcID, _ := a.Mapper().Lookup(src)
		gotDist, err := Dijkstra(c, srcID)
		if err != nil {
			t.Fatalf("seed=%d: Dijkstra: %v", seed, err)
		}
		// Naive Bellman-Ford reference.
		ref := bellmanFordRef(edges, src, n)
		for v := 0; v < n; v++ {
			nodeID, okm := a.Mapper().Lookup(v)
			if !okm {
				if ref[v] >= 0 {
					t.Fatalf("seed=%d: node %d in ref but not Mapper", seed, v)
				}
				continue
			}
			got, gok := gotDist.Distance(nodeID)
			if ref[v] < 0 {
				if gok {
					t.Fatalf("seed=%d: Dijkstra distance %d for %d unreachable in ref", seed, got, v)
				}
				continue
			}
			if !gok || int64(got) != ref[v] {
				t.Fatalf("seed=%d: node %d Dijkstra=(%d,%v) ref=%d", seed, v, got, gok, ref[v])
			}
		}
	}
}

// bellmanFordRef returns shortest distances from src to every node in
// [0, n), or -1 when unreachable. Negative weights would yield
// undefined results — this reference accepts the same non-negative
// input as the Dijkstra runs we compare against.
func bellmanFordRef(edges []weightedEdge, src, n int) []int64 {
	const inf int64 = 1 << 60
	d := make([]int64, n)
	for i := range d {
		d[i] = inf
	}
	d[src] = 0
	for i := 0; i < n-1; i++ {
		changed := false
		for _, e := range edges {
			if d[e.from] != inf && d[e.from]+e.w < d[e.to] {
				d[e.to] = d[e.from] + e.w
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	out := make([]int64, n)
	for i, v := range d {
		if v == inf {
			out[i] = -1
		} else {
			out[i] = v
		}
	}
	return out
}

func BenchmarkDijkstra_RandomGraph(b *testing.B) {
	a := adjlist.New[uint32, int64](adjlist.Config{Directed: true})
	const universe = 1 << 20 // 1M nodes
	for i := uint32(0); i < uint32(universe); i++ {
		a.AddNode(i)
	}
	r := rand.New(rand.NewPCG(31, 1)) //nolint:gosec // deterministic benchmark RNG
	const fill = 1 << 22              // 4M edges
	for i := 0; i < fill; i++ {
		a.AddEdge(uint32(r.IntN(universe)), uint32(r.IntN(universe)), int64(r.IntN(100)+1))
	}
	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(uint32(0))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Dijkstra(c, srcID)
	}
}
