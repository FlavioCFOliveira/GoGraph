package centrality

import (
	"context"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestBrandesDistanceHoist_BitIdentical pins the inner-loop change of rmp
// #2868 — dist[v]+1 hoisted out of the neighbour loop — against
// betweennessLegacy, the unchanged pre-arena reference implementation
// that still computes dist[v]+1 per edge.
//
// The shapes are chosen for the ways the hoist could diverge, and for the
// risk of the dist[w] cache that the same #2868 split measured and
// dropped (see brandes.go). The cache is gone; the shapes stay, because
// they are now what pins that the hoist alone is still exact on them:
//
//   - a self-loop makes w == v inside the loop, the only case in which
//     the "discovered" arm could write the dist[v] the hoisted sum was
//     taken from;
//   - parallel edges make the same w appear twice in one adjacency run,
//     so dist[w] is read again after the loop itself wrote it — the case
//     that condemned a cached copy, and here the case that proves the
//     surviving reload agrees with the hoisted dv1;
//   - equal-distance predecessors are what make the accumulation order
//     observable at all, so every shape carries several.
func TestBrandesDistanceHoist_BitIdentical(t *testing.T) {
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
			// Every vertex carries a self-loop, so w == v arises at every
			// single source.
			name:  "self-loop-on-every-vertex",
			edges: [][2]int{{0, 0}, {1, 1}, {2, 2}, {3, 3}, {0, 1}, {1, 2}, {2, 3}, {0, 3}},
		},
		{
			// The same neighbour repeated three times in one run, so
			// dist[w] is read twice more after being written.
			name:  "triple-parallel-edge",
			edges: [][2]int{{0, 1}, {0, 1}, {0, 1}, {1, 2}, {1, 2}, {2, 3}, {0, 2}},
		},
		{
			name:     "directed-self-loop-and-parallel",
			directed: true,
			edges:    [][2]int{{0, 0}, {0, 1}, {0, 1}, {1, 1}, {1, 2}, {2, 0}, {0, 2}},
		},
		{
			// A wide equal-distance layer: node 0 reaches 1..6 at depth 1
			// and 7 at depth 2 through six equal shortest paths, so delta
			// accumulates six non-associative terms per source.
			name:  "six-way-equal-distance",
			edges: [][2]int{{0, 1}, {0, 2}, {0, 3}, {0, 4}, {0, 5}, {0, 6}, {1, 7}, {2, 7}, {3, 7}, {4, 7}, {5, 7}, {6, 7}},
		},
		{
			name:  "single-vertex-self-loop",
			edges: [][2]int{{0, 0}},
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
				t.Fatalf("serial not bit-identical at node %d: legacy=%v hoisted=%v",
					idx, want[idx], got[idx])
			}
			// brandesSource has two call sites — the serial walk above and
			// the per-worker walk below. Both must be pinned, or half the
			// fix is unverified.
			if runtime.GOMAXPROCS(0) < 2 {
				return
			}
			par, err := BetweennessParallelCtx(context.Background(), c, 4)
			if err != nil {
				t.Fatalf("BetweennessParallelCtx: %v", err)
			}
			if idx, ok := bitsEqual(want, par); !ok {
				t.Fatalf("parallel not bit-identical at node %d: legacy=%v hoisted=%v",
					idx, want[idx], par[idx])
			}
		})
	}
}

// TestBrandesDistanceHoist_BitIdentical_MultigraphFuzz fuzzes dense
// random multigraphs — many parallel edges and self-loops per vertex, so
// every source hits both w == v and a re-read of a dist[w] the loop just
// wrote — and asserts bit-identity against the legacy reference on each.
func TestBrandesDistanceHoist_BitIdentical_MultigraphFuzz(t *testing.T) {
	t.Parallel()
	for _, directed := range []bool{false, true} {
		for seed := uint64(1); seed <= 12; seed++ {
			directed, seed := directed, seed
			name := "undirected"
			if directed {
				name = "directed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				const n = 40
				//nolint:gosec // G404: math/rand/v2 PCG seeded from the test's own parameter; this test asserts a reproducible shape, which a CSPRNG would destroy.
				r := rand.New(rand.NewPCG(seed, seed*2246822519))
				a := adjlist.New[int, struct{}](adjlist.Config{Directed: directed})
				// Deliberately dense in duplicates: 8n edges over n
				// vertices draws the same (u,v) pair many times, and
				// u == v often.
				for i := 0; i < 8*n; i++ {
					_ = a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
				}
				c := csr.BuildFromAdjList(a)
				want := betweennessLegacy(c)
				got := Betweenness(c)
				if idx, ok := bitsEqual(want, got); !ok {
					t.Fatalf("seed %d not bit-identical at node %d: legacy=%v hoisted=%v",
						seed, idx, want[idx], got[idx])
				}
			})
		}
	}
}
