package search

import (
	"math/rand/v2"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// BenchmarkBFSDO_VsTopDown_PowerLaw compares BFS-DO against the
// vanilla top-down [BFS] on a power-law-flavoured graph. Task #129
// requires BFS-DO to beat top-down by >3x at peak; the alpha/beta
// switch should keep bottom-up active for the dense middle and
// return to top-down for the sparse tail.
func BenchmarkBFSDO_VsTopDown_PowerLaw(b *testing.B) {
	c, srcID := powerLawCSR(b)
	b.Run("TopDown", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			BFS(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
		}
	})
	b.Run("DirectionOpt", func(b *testing.B) {
		BFSDirectionOpt(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			BFSDirectionOpt(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
		}
	})
}

func powerLawCSR(tb testing.TB) (*csr.CSR[struct{}], graph.NodeID) {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 1 << 20                  // 1M nodes
	r := rand.New(rand.NewPCG(53, 59)) //nolint:gosec // deterministic benchmark RNG
	const edgesPerNode = 16
	// Heavy-tailed power-law: cube of a uniform sample biases sharply
	// toward low ids, planting hubs at the front of the universe.
	for i := 0; i < n*edgesPerNode; i++ {
		from := int(float64(n) * r.Float64() * r.Float64() * r.Float64())
		to := int(float64(n) * r.Float64() * r.Float64() * r.Float64())
		if from >= n {
			from = n - 1
		}
		if to >= n {
			to = n - 1
		}
		if from == to {
			continue
		}
		if err := a.AddEdge(from, to, struct{}{}); err != nil {
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	return c, src
}

// BenchmarkBFSDirectionOpt_PowerLaw exercises BFS-DO on a power-law-
// flavoured undirected random graph where the algorithm benefits
// most. The bench-loop reuses pooled scratch across iterations, so
// allocs/op should be 0 post-warmup.
func BenchmarkBFSDirectionOpt_PowerLaw(b *testing.B) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 1 << 20                  // 1M nodes
	r := rand.New(rand.NewPCG(53, 59)) //nolint:gosec // deterministic benchmark RNG
	// Power-law-ish: hubs (low indices) receive far more edges.
	const edgesPerNode = 4
	for i := 0; i < n*edgesPerNode; i++ {
		from := r.IntN(n)
		// Sample destination biased toward the low-id hubs.
		to := int(float64(n) * r.Float64() * r.Float64())
		if to >= n {
			to = n - 1
		}
		if err := a.AddEdge(from, to, struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	// Warm pool.
	BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool { return true })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool { return true })
	}
}

func TestBFSDirectionOpt_Tree(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 4}, {1, 5}, {3, 6}}
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	depths := map[int]int{}
	BFSDirectionOpt(c, src, func(node graph.NodeID, d int) bool {
		v, _ := a.Mapper().Resolve(node)
		depths[v] = d
		return true
	})
	want := map[int]int{0: 0, 1: 1, 2: 1, 3: 1, 4: 2, 5: 2, 6: 2}
	for k, v := range want {
		if depths[k] != v {
			t.Fatalf("depth[%d] = %d, want %d", k, depths[k], v)
		}
	}
}

func TestBFSDirectionOpt_AllReachable(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 9; i++ {
		if err := a.AddEdge(0, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	visited := 0
	BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	if visited != 10 {
		t.Fatalf("visited = %d, want 10", visited)
	}
}

// TestBFSDirectionOpt_BetaSwitchBack asserts Beamer's alpha/beta
// regime: on a power-law-flavoured graph the search visits at least
// one top-down step, then transitions to bottom-up for the dense
// middle, then transitions back to top-down for the sparse tail.
// The observer hook records the (depth, mode) pair for every step.
func TestBFSDirectionOpt_BetaSwitchBack(t *testing.T) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 4096
	// Dense ball + sparse tail: src reaches a clique of size 200 at
	// depth 1; the clique exits via a single edge to a long chain.
	// The clique step triggers alpha (frontier saturates), and the
	// chain step triggers beta (|cur| collapses to 1, well under
	// maxID/24 = 170).
	for i := 1; i < 200; i++ {
		if err := a.AddEdge(0, i, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for i := 1; i < 200; i++ {
		for j := i + 1; j < 200; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	if err := a.AddEdge(199, 200, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	for i := 200; i < n-1; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	var steps []struct {
		depth      int
		isBottomUp bool
	}
	obs := func(depth int, isBottomUp bool) {
		steps = append(steps, struct {
			depth      int
			isBottomUp bool
		}{depth, isBottomUp})
	}

	bfsDoCore(c, src, func(_ graph.NodeID, _ int) bool { return true }, obs)

	if len(steps) < 3 {
		t.Fatalf("expected >= 3 BFS-DO steps, got %d", len(steps))
	}
	sawTopDown := false
	sawBottomUp := false
	sawTopDownAfterBottomUp := false
	for _, s := range steps {
		if !s.isBottomUp {
			sawTopDown = true
			if sawBottomUp {
				sawTopDownAfterBottomUp = true
			}
		} else {
			sawBottomUp = true
		}
	}
	if !sawTopDown {
		t.Fatalf("never ran a top-down step on a power-law-like graph")
	}
	if !sawBottomUp {
		t.Fatalf("never ran a bottom-up step on a power-law-like graph")
	}
	if !sawTopDownAfterBottomUp {
		t.Fatalf("beta switch-back not exercised: bottom-up never followed by top-down")
	}
}

func TestBFSDirectionOpt_EarlyStop(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 100; i++ {
		if err := a.AddEdge(0, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	visited := 0
	BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool {
		visited++
		return visited < 5
	})
	if visited != 5 {
		t.Fatalf("early-stop visited = %d", visited)
	}
}
