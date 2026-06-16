package centrality

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// betweennessLegacy is a faithful copy of the pre-#1515 Brandes
// implementation that used a slice-of-slices (`pred [][]int`) for the
// predecessor sets. It exists solely so the new flat-arena variant
// can be proven to produce bit-identical betweenness scores: the
// arena preserves the exact predecessor insertion order, so the
// non-associative floating-point dependency sums are identical down
// to the last bit. Any divergence here is a correctness regression.
func betweennessLegacy[W any](c *csr.CSR[W]) []float64 {
	maxID := int(c.MaxNodeID())
	cb := make([]float64, maxID)
	if maxID == 0 {
		return cb
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	sigma := make([]float64, maxID)
	dist := make([]int, maxID)
	delta := make([]float64, maxID)
	pred := make([][]int, maxID)
	queue := make([]int, 0, maxID)
	stack := make([]int, 0, maxID)

	for s := 0; s < maxID; s++ {
		queue, stack = brandesSourceLegacy(s, maxID, verts, edges, sigma, dist, delta, pred, cb, queue, stack)
	}
	return cb
}

func brandesSourceLegacy(s, maxID int, verts []uint64, edges []graph.NodeID, sigma []float64, dist []int, delta []float64, pred [][]int, cb []float64, queue, stack []int) (queueOut, stackOut []int) {
	for i := 0; i < maxID; i++ {
		sigma[i] = 0
		dist[i] = -1
		delta[i] = 0
		pred[i] = pred[i][:0]
	}
	sigma[s] = 1
	dist[s] = 0
	queue = append(queue[:0], s)
	stack = stack[:0]
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		stack = append(stack, v)
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			if dist[w] < 0 {
				dist[w] = dist[v] + 1
				queue = append(queue, w)
			}
			if dist[w] == dist[v]+1 {
				sigma[w] += sigma[v]
				pred[w] = append(pred[w], v)
			}
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		w := stack[i]
		for _, v := range pred[w] {
			delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
		}
		if w != s {
			cb[w] += delta[w]
		}
	}
	return queue, stack
}

// bitsEqual reports whether every element of a and b has the same
// IEEE-754 bit pattern (so 0.0 and -0.0 differ and NaNs compare by
// payload). This is stricter than ==; it is the right tool to prove
// the arena change did not perturb any floating-point sum.
func bitsEqual(a, b []float64) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return i, false
		}
	}
	return -1, true
}

// TestBrandesArena_BitIdentical proves the flat-arena predecessor
// store (#1515) yields betweenness scores bit-identical to the
// legacy slice-of-slices implementation across a battery of graph
// shapes: directed and undirected, dense and sparse, disconnected,
// and a self-loop / multigraph case.
func TestBrandesArena_BitIdentical(t *testing.T) {
	t.Parallel()

	build := func(directed bool, edges [][2]int) *csr.CSR[struct{}] {
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: directed})
		for _, e := range edges {
			if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
				t.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
			}
		}
		return csr.BuildFromAdjList(a)
	}

	cases := []struct {
		name     string
		directed bool
		edges    [][2]int
	}{
		{
			name:  "undirected-path",
			edges: [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}},
		},
		{
			name:  "undirected-star",
			edges: [][2]int{{0, 1}, {0, 2}, {0, 3}, {0, 4}},
		},
		{
			name:  "undirected-diamond-multipath",
			edges: [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}, {3, 5}, {4, 6}, {5, 6}},
		},
		{
			name:     "directed-dag",
			directed: true,
			edges:    [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}},
		},
		{
			name:     "directed-cycle",
			directed: true,
			edges:    [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 0}, {0, 2}},
		},
		{
			name:  "disconnected-two-components",
			edges: [][2]int{{0, 1}, {1, 2}, {5, 6}, {6, 7}, {7, 5}},
		},
		{
			name:  "self-loop-and-parallel",
			edges: [][2]int{{0, 0}, {0, 1}, {0, 1}, {1, 2}, {2, 0}},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := build(tc.directed, tc.edges)
			want := betweennessLegacy(c)
			got := Betweenness(c)
			if idx, ok := bitsEqual(want, got); !ok {
				t.Fatalf("not bit-identical at node %d: legacy=%v arena=%v\nlegacy=%v\narena =%v",
					idx, want[idx], got[idx], want, got)
			}
		})
	}
}

// TestBrandesArena_BitIdentical_Random fuzzes a batch of random
// directed and undirected graphs (the shape exercised by the guard-
// band benchmark) and asserts bit-identity on every one. Several
// equal-distance predecessors per vertex is the case that makes the
// accumulation order observable, so dense random graphs are the
// strongest evidence.
func TestBrandesArena_BitIdentical_Random(t *testing.T) {
	t.Parallel()
	for _, directed := range []bool{false, true} {
		for seed := uint64(1); seed <= 16; seed++ {
			directed, seed := directed, seed
			name := "undirected"
			if directed {
				name = "directed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				const n = 64
				r := rand.New(rand.NewPCG(seed, seed*2654435761))
				a := adjlist.New[int, struct{}](adjlist.Config{Directed: directed})
				for i := 0; i < 4*n; i++ {
					_ = a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
				}
				c := csr.BuildFromAdjList(a)
				want := betweennessLegacy(c)
				got := Betweenness(c)
				if idx, ok := bitsEqual(want, got); !ok {
					t.Fatalf("seed %d not bit-identical at node %d: legacy=%v arena=%v",
						seed, idx, want[idx], got[idx])
				}
			})
		}
	}
}
