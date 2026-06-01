package search

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildFloat64CSR builds a directed graph with float64 edge weights
// using the provided edge list. Nodes are keyed by int (0-based).
func buildFloat64CSR(tb testing.TB, edges []float64Edge, directed bool) (*csr.CSR[float64], *adjlist.AdjList[int, float64]) {
	tb.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: directed})
	for _, e := range edges {
		if err := a.AddEdge(e.from, e.to, e.w); err != nil {
			tb.Fatalf("AddEdge(%d→%d, %v): %v", e.from, e.to, e.w, err)
		}
	}
	return csr.BuildFromAdjList(a), a
}

type float64Edge struct {
	from, to int
	w        float64
}

// TestDijkstra_Kn verifies that Dijkstra's SSSP from node 0 on a
// complete directed graph Kn with random non-negative float64 weights
// matches the APSP row from DijkstraAPSP (used as a reference).
//
// DijkstraAPSP runs a full APSP over the same CSR; comparing its
// (src=0, dst=*) slice against Dijkstra's single-source distances
// exercises every relaxation path for both n=8 and n=32.
func TestDijkstra_Kn(t *testing.T) {
	t.Parallel()

	for _, n := range []int{8, 32} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			rng := rand.New(rand.NewPCG(0xDEAD_BEEF, uint64(n))) //nolint:gosec // deterministic benchmark/test RNG; crypto entropy not required

			// Build K_n with random non-negative weights.
			edges := make([]float64Edge, 0, n*(n-1))
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					if i == j {
						continue
					}
					// Weight in [0, 100).
					w := rng.Float64() * 100.0
					edges = append(edges, float64Edge{from: i, to: j, w: w})
				}
			}

			c, a := buildFloat64CSR(t, edges, true)

			src, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatal("node 0 not found in mapper")
			}

			// Single-source Dijkstra from node 0.
			dijk, err := Dijkstra(c, src)
			if err != nil {
				t.Fatalf("Dijkstra: %v", err)
			}

			// APSP reference via DijkstraAPSP.
			apsp, err := DijkstraAPSP(c)
			if err != nil {
				t.Fatalf("DijkstraAPSP: %v", err)
			}

			for key := 0; key < n; key++ {
				nodeID, _ := a.Mapper().Lookup(key)

				dijDist, dijOK := dijk.Distance(nodeID)
				apspDist, apspOK := apsp.At(src, nodeID)

				if dijOK != apspOK {
					t.Errorf("node %d: Dijkstra reachable=%v, APSP reachable=%v", key, dijOK, apspOK)
					continue
				}
				if !dijOK {
					continue
				}
				if dijDist != apspDist {
					t.Errorf("node %d: Dijkstra dist=%v, APSP dist=%v", key, dijDist, apspDist)
				}
			}
		})
	}
}

// TestDijkstra_Kn_Determinism verifies that two consecutive Dijkstra
// runs on the same graph produce bit-identical distances.
func TestDijkstra_Kn_Determinism(t *testing.T) {
	t.Parallel()

	const n = 16
	rng := rand.New(rand.NewPCG(0xCAFE, 0xBABE)) //nolint:gosec // deterministic test RNG; crypto entropy not required
	edges := make([]float64Edge, 0, n*(n-1))
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				edges = append(edges, float64Edge{i, j, rng.Float64() * 50.0})
			}
		}
	}
	c, a := buildFloat64CSR(t, edges, true)
	src, _ := a.Mapper().Lookup(0)

	d1, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("first Dijkstra: %v", err)
	}
	d2, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("second Dijkstra: %v", err)
	}
	for i := 0; i < n; i++ {
		id := graph.NodeID(i)
		v1, ok1 := d1.Distance(id)
		v2, ok2 := d2.Distance(id)
		if ok1 != ok2 || v1 != v2 {
			t.Errorf("node %d: run1=(%v,%v) run2=(%v,%v)", i, v1, ok1, v2, ok2)
		}
	}
}
