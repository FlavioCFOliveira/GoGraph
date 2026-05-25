package search

// Task 567: BFS on hypercube Q_d.
//
// shapegen.Hypercube(d) builds an undirected Q_d with 2^d vertices.
// Vertex u has key equal to the integer u (binary representation).
// From vertex 0 (key=0), dist[v] == popcount(v) == number of set bits.
//
// Verified by checking the binomial distribution of distances:
//
//	|{v : dist[v] == k}| == C(d, k)  for k in [0, d].
//
// Also verifies: max(dist) == d.

import (
	"math/bits"
	"testing"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestBFS_Hypercube_BinomialDistances(t *testing.T) {
	t.Parallel()
	for _, d := range []int{3, 5, 10} {
		d := d
		t.Run("d="+itoa(d), func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.Hypercube(d).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			n := 1 << d
			srcID, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("key 0 not found in mapper")
			}
			dist := make(map[int]int, n)
			BFS(c, srcID, func(node graph.NodeID, depth int) bool {
				v, vok := a.Mapper().Resolve(node)
				if !vok {
					t.Errorf("resolve failed for NodeID %d", node)
					return false
				}
				dist[v] = depth
				return true
			})
			if len(dist) != n {
				t.Fatalf("visited %d nodes, want %d", len(dist), n)
			}

			maxDist := 0
			for _, depth := range dist {
				if depth > maxDist {
					maxDist = depth
				}
			}
			if maxDist != d {
				t.Fatalf("max dist = %d, want %d (= d)", maxDist, d)
			}

			// Verify per-distance bin sizes against C(d, k).
			count := make([]int, d+1)
			for v, depth := range dist {
				// dist[v] must equal popcount(v) for Hypercube from vertex 0.
				want := bits.OnesCount(uint(v))
				if depth != want {
					t.Errorf("dist[%d] = %d, want popcount(%d) = %d", v, depth, v, want)
				}
				count[depth]++
			}
			for k := 0; k <= d; k++ {
				wantCount := binomial(d, k)
				if count[k] != wantCount {
					t.Fatalf("at dist %d: %d nodes, want C(%d,%d)=%d", k, count[k], d, k, wantCount)
				}
			}
		})
	}
}

// binomial computes C(n, k) iteratively. n and k are small (d<=10),
// so integer overflow is not a concern.
func binomial(n, k int) int {
	if k > n {
		return 0
	}
	if k == 0 || k == n {
		return 1
	}
	// Use the smaller of k and n-k for efficiency.
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 0; i < k; i++ {
		result = result * (n - i) / (i + 1)
	}
	return result
}
