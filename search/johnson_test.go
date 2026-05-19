package search

import (
	"errors"
	"math/rand/v2"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestJohnson_MatchesFloydWarshall(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	apspJ, err := JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("JohnsonAPSP: %v", err)
	}
	apspF := FloydWarshall(c)
	if apspJ.N() != apspF.N() {
		t.Fatalf("size mismatch")
	}
	for i := 0; i < apspJ.N(); i++ {
		for j := 0; j < apspJ.N(); j++ {
			vJ, okJ := apspJ.At(uint64ToNodeID(i), uint64ToNodeID(j))
			vF, okF := apspF.At(uint64ToNodeID(i), uint64ToNodeID(j))
			if okJ != okF {
				t.Fatalf("reachability mismatch at (%d,%d)", i, j)
			}
			if okJ && vJ != vF {
				t.Fatalf("(%d,%d): Johnson=%d Floyd=%d", i, j, vJ, vF)
			}
		}
	}
}

// TestDijkstraAPSP_PrimaryAPI exercises the non-negative fast-path
// entrypoint with a simple fixture.
func TestDijkstraAPSP_PrimaryAPI(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 4}, {0, 2, 1},
		{2, 1, 2}, {1, 3, 1},
		{2, 3, 5},
	})
	apsp, err := DijkstraAPSP(c)
	if err != nil {
		t.Fatalf("DijkstraAPSP: %v", err)
	}
	if apsp.N() != 4 {
		t.Fatalf("APSP.N() = %d, want 4", apsp.N())
	}
}

// TestDijkstraAPSP_NegativeEdgeRejected verifies non-negative-weight
// precondition is enforced with a typed sentinel.
func TestDijkstraAPSP_NegativeEdgeRejected(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, -1},
		{1, 2, 2},
	})
	_, err := DijkstraAPSP(c)
	if !errors.Is(err, ErrNegativeEdgeAPSP) {
		t.Fatalf("DijkstraAPSP on negative edge: err=%v want ErrNegativeEdgeAPSP", err)
	}
}

// TestJohnsonAPSP_NegativeWeights exercises the textbook CLRS
// Bellman-Ford fixture (Fig. 25.1) which has mixed-sign weights and
// no negative cycle. The Johnson output must match Floyd-Warshall
// cell-by-cell across every (i, j) pair.
func TestJohnsonAPSP_NegativeWeights(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 3}, {0, 2, 8}, {0, 4, -4},
		{1, 3, 1}, {1, 4, 7},
		{2, 1, 4},
		{3, 0, 2}, {3, 2, -5},
		{4, 3, 6},
	})
	apspJ, err := JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("JohnsonAPSP: %v", err)
	}
	apspF := FloydWarshall(c)
	if apspJ.N() != apspF.N() {
		t.Fatalf("size mismatch: Johnson=%d Floyd=%d", apspJ.N(), apspF.N())
	}
	for i := 0; i < apspJ.N(); i++ {
		for j := 0; j < apspJ.N(); j++ {
			vJ, okJ := apspJ.At(uint64ToNodeID(i), uint64ToNodeID(j))
			vF, okF := apspF.At(uint64ToNodeID(i), uint64ToNodeID(j))
			if okJ != okF {
				t.Fatalf("reachability mismatch at (%d,%d): Johnson=%v Floyd=%v", i, j, okJ, okF)
			}
			if okJ && vJ != vF {
				t.Fatalf("(%d,%d): Johnson=%d Floyd=%d", i, j, vJ, vF)
			}
		}
	}
}

// TestJohnsonAPSP_NegativeCycleDetected confirms a negative cycle is
// surfaced through ErrNegativeCycle. The fixture is the same 3-node
// cycle 0 -> 1 -> 2 -> 0 with total weight -1 used by the
// Bellman-Ford negative-cycle test.
func TestJohnsonAPSP_NegativeCycleDetected(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR([]weightedEdge{
		{0, 1, 1}, {1, 2, -3}, {2, 0, 1},
	})
	_, err := JohnsonAPSP(c)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("JohnsonAPSP on negative cycle: err=%v want ErrNegativeCycle", err)
	}
}

// TestJohnsonAPSP_RandomVsFloydWarshall_Property generates random
// graphs with mixed-sign integer weights and asserts that whenever
// Johnson succeeds (i.e. no negative cycle), every (i, j) cell
// matches Floyd-Warshall exactly. Integer weights guarantee exact
// arithmetic; floating-point parity is documented in the JohnsonAPSP
// godoc.
func TestJohnsonAPSP_RandomVsFloydWarshall_Property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 10).Draw(r, "n")
		m := rapid.IntRange(0, 3*n).Draw(r, "m")
		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			a.AddNode(i)
		}
		for i := 0; i < m; i++ {
			u := rapid.IntRange(0, n-1).Draw(r, "u")
			v := rapid.IntRange(0, n-1).Draw(r, "v")
			// Mixed-sign weights with a strong positive bias so the
			// random fixture rarely admits a negative cycle.
			w := int64(rapid.IntRange(-3, 20).Draw(r, "w"))
			a.AddEdge(u, v, w)
		}
		c := csr.BuildFromAdjList(a)
		apspJ, err := JohnsonAPSP(c)
		if err != nil {
			// Negative cycle: skip — Floyd-Warshall does not detect
			// negative cycles, so cross-checking it would compare
			// against an undefined output.
			if !errors.Is(err, ErrNegativeCycle) {
				r.Fatalf("JohnsonAPSP: unexpected error %v", err)
			}
			return
		}
		apspF := FloydWarshall(c)
		if apspJ.N() != apspF.N() {
			r.Fatalf("size mismatch: Johnson=%d Floyd=%d", apspJ.N(), apspF.N())
		}
		for i := 0; i < apspJ.N(); i++ {
			for j := 0; j < apspJ.N(); j++ {
				vJ, okJ := apspJ.At(uint64ToNodeID(i), uint64ToNodeID(j))
				vF, okF := apspF.At(uint64ToNodeID(i), uint64ToNodeID(j))
				if okJ != okF {
					r.Fatalf("(%d,%d): reachability Johnson=%v Floyd=%v", i, j, okJ, okF)
				}
				if okJ && vJ != vF {
					r.Fatalf("(%d,%d): Johnson=%d Floyd=%d", i, j, vJ, vF)
				}
			}
		}
	})
}

// sparseRandomCSR builds a deterministic sparse weighted CSR with
// roughly E = 2*V edges. negativeFraction is the fraction of edges
// that receive a (small) negative weight; 0 means strictly positive.
func sparseRandomCSR(t testing.TB, n int, seed1, seed2 uint64, negativeFraction float64) *csr.CSR[int64] {
	t.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		a.AddNode(i)
	}
	r := rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // deterministic benchmark RNG
	edges := 2 * n
	for i := 0; i < edges; i++ {
		u := r.IntN(n)
		v := r.IntN(n)
		var w int64
		if negativeFraction > 0 && r.Float64() < negativeFraction {
			w = -int64(r.IntN(3) + 1)
		} else {
			w = int64(r.IntN(50) + 5)
		}
		a.AddEdge(u, v, w)
	}
	return csr.BuildFromAdjList(a)
}

// BenchmarkJohnsonAPSP_Sparse exercises Johnson on a V=512, E~=2V
// graph with strictly positive weights, the regime where Johnson's
// O(V * (V + E) log V) should beat Floyd-Warshall's O(V^3).
func BenchmarkJohnsonAPSP_Sparse(b *testing.B) {
	c := sparseRandomCSR(b, 512, 131, 173, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := JohnsonAPSP(c)
		if err != nil {
			b.Fatalf("JohnsonAPSP: %v", err)
		}
	}
}

// BenchmarkFloydWarshall_Sparse mirrors BenchmarkJohnsonAPSP_Sparse
// on the same V=512, E~=2V fixture so benchstat can directly compare
// the two algorithms on the sparse-graph regime.
func BenchmarkFloydWarshall_Sparse(b *testing.B) {
	c := sparseRandomCSR(b, 512, 131, 173, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FloydWarshall(c)
	}
}

// BenchmarkJohnsonAPSP_NegativeWeights mirrors the sparse setup with
// ~10% of edges carrying a negative weight, exercising the
// Bellman-Ford reweighting cost on a representative sparse input.
func BenchmarkJohnsonAPSP_NegativeWeights(b *testing.B) {
	c := sparseRandomCSR(b, 512, 211, 313, 0.1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := JohnsonAPSP(c)
		if err != nil {
			if errors.Is(err, ErrNegativeCycle) {
				b.Skip("random fixture produced a negative cycle; retry with different seeds")
				return
			}
			b.Fatalf("JohnsonAPSP: %v", err)
		}
	}
}
