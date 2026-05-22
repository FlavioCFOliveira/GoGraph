package search

import (
	"errors"
	"math/rand/v2"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBidirectionalDijkstra_HandBuilt(t *testing.T) {
	t.Parallel()
	// CLRS-style: shortest path 0->2->4 costs 5.
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(4)
	path, cost, err := BidirectionalDijkstra(c, src, dst)
	if err != nil {
		t.Fatalf("BiDijkstra: %v", err)
	}
	if cost != 5 {
		t.Fatalf("cost = %d, want 5", cost)
	}
	if len(path) != 3 {
		t.Fatalf("path length = %d, want 3", len(path))
	}
	if path[0] != src || path[len(path)-1] != dst {
		t.Fatalf("path endpoints wrong: %v", path)
	}
}

func TestBidirectionalDijkstra_SameSrcDst(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 5}})
	src, _ := a.Mapper().Lookup(0)
	path, cost, err := BidirectionalDijkstra(c, src, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(path) != 1 || cost != 0 {
		t.Fatalf("self-path = %v cost=%d, want [src] 0", path, cost)
	}
}

func TestBidirectionalDijkstra_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	_, _, err := BidirectionalDijkstra(c, src, dst)
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("expected ErrNoPath, got %v", err)
	}
}

func TestBidirectionalDijkstra_NegativeWeight(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{{0, 1, -3}})
	_, _, err := BidirectionalDijkstra(c, 0, 1)
	if !errors.Is(err, ErrNegativeWeight) {
		t.Fatalf("expected ErrNegativeWeight, got %v", err)
	}
}

// TestBidirectionalDijkstra_RandomVsDijkstra is the rapid-style
// property test from #141 acceptance: on random non-negative-weight
// graphs the cost of the path returned by BidirectionalDijkstra must
// equal the [Dijkstra] s->t distance.
func TestBidirectionalDijkstra_RandomVsDijkstra(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewPCG(seed, 23)) //nolint:gosec // deterministic
		const n = 64
		const e = 200
		edges := make([]weightedEdge, 0, e)
		for i := 0; i < e; i++ {
			edges = append(edges, weightedEdge{
				from: r.IntN(n),
				to:   r.IntN(n),
				w:    int64(r.IntN(40) + 1),
			})
		}
		c, a := buildWeightedCSRCfg(t, edges, adjlist.Config{Directed: true, Multigraph: true})
		src := r.IntN(n)
		dst := r.IntN(n)
		srcID, ok1 := a.Mapper().Lookup(src)
		dstID, ok2 := a.Mapper().Lookup(dst)
		if !ok1 || !ok2 {
			continue
		}
		d, err := Dijkstra(c, srcID)
		if err != nil {
			t.Fatalf("seed=%d Dijkstra: %v", seed, err)
		}
		dijDist, dijOK := d.Distance(dstID)
		_, _, biErr := BidirectionalDijkstra(c, srcID, dstID)
		path, biCost, _ := BidirectionalDijkstra(c, srcID, dstID)
		if dijOK {
			if biErr != nil {
				t.Fatalf("seed=%d: BiDijkstra returned %v but Dijkstra found path of cost %d", seed, biErr, dijDist)
			}
			if biCost != dijDist {
				t.Fatalf("seed=%d: BiDijkstra cost = %d, Dijkstra cost = %d", seed, biCost, dijDist)
			}
			if path[0] != srcID || path[len(path)-1] != dstID {
				t.Fatalf("seed=%d: path endpoints wrong: %v", seed, path)
			}
		} else if !errors.Is(biErr, ErrNoPath) {
			t.Fatalf("seed=%d: BiDijkstra found path but Dijkstra reported unreachable", seed)
		}
	}
}

// BenchmarkBidirectionalDijkstra_RoadNetwork measures BiDijkstra on
// a grid-shaped, low-degree, geometrically-spread graph that mimics
// a road network with random point-to-point queries — the regime
// where bidirectional Dijkstra wins. Each iteration uses a different
// (src, dst) pair, so Dijkstra cannot amortise its full SSSP work
// across queries. The reverse CSR is built once outside the loop.
// Task #141 targets >2x speedup over the one-way [Dijkstra] baseline.
func BenchmarkBidirectionalDijkstra_RoadNetwork(b *testing.B) {
	c, _, _ := buildRoadNetwork(b)
	rev := c.BuildReverse()
	const side = 200
	const queries = 64
	srcs := make([]graph.NodeID, queries)
	dsts := make([]graph.NodeID, queries)
	r := rand.New(rand.NewPCG(101, 103)) //nolint:gosec // deterministic
	for i := 0; i < queries; i++ {
		srcs[i] = graph.NodeID(r.IntN(side * side))
		dsts[i] = graph.NodeID(r.IntN(side * side))
	}
	b.Run("Dijkstra", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			q := i % queries
			d, _ := Dijkstra(c, srcs[q])
			_, _ = d.Distance(dsts[q])
		}
	})
	b.Run("BiDijkstra", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			q := i % queries
			_, _, _ = BidirectionalDijkstraOn(c, rev, srcs[q], dsts[q])
		}
	})
}

func buildRoadNetwork(tb testing.TB) (c *csr.CSR[int64], src, dst graph.NodeID) {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	const side = 200 // 200x200 = 40k nodes
	for r := 0; r < side; r++ {
		for c := 0; c < side; c++ {
			cur := r*side + c
			if c+1 < side {
				if err := a.AddEdge(cur, r*side+c+1, int64(1+(r+c)%5)); err != nil {
					tb.Fatalf("AddEdge: %v", err)
				}
			}
			if r+1 < side {
				if err := a.AddEdge(cur, (r+1)*side+c, int64(1+(r+c)%5)); err != nil {
					tb.Fatalf("AddEdge: %v", err)
				}
			}
		}
	}
	c = csr.BuildFromAdjList(a)
	src, _ = a.Mapper().Lookup(0)
	dst, _ = a.Mapper().Lookup(side*side - 1)
	return c, src, dst
}
