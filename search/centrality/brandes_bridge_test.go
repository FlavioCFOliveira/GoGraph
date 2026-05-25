package centrality

import (
	"math"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBetweenness_BridgeCluster builds an undirected graph of two
// complete cliques K_k joined by a single bridge edge and asserts
// that the two bridge endpoints are the unique top-2 ranked vertices
// by betweenness, with strictly greater values than all other nodes.
//
// Topology (k=20):
//
//	clique A: vertices 0..k-1
//	clique B: vertices k..2k-1
//	bridge:   edge (k-1, k)
//
// Every shortest path between a node in A and a node in B must
// traverse the bridge, so the endpoints k-1 and k accumulate
// betweenness from k² off-cluster source–target pairs.
func TestBetweenness_BridgeCluster(t *testing.T) {
	t.Parallel()
	const k = 20
	bc, bridgeA, bridgeB := buildBridgeGraph(t, k)
	assertBridgeDominance(t, bc, bridgeA, bridgeB)
}

// TestBetweenness_BridgeCluster_Rapid sweeps k in [5, 30] and
// checks the bridge-dominance invariant holds for every size.
func TestBetweenness_BridgeCluster_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		k := rapid.IntRange(5, 30).Draw(rt, "k")
		bc, bridgeA, bridgeB := buildBridgeGraph(t, k)
		assertBridgeDominance(rt, bc, bridgeA, bridgeB)
	})
}

// buildBridgeGraph constructs the double-clique bridge graph for the
// given clique size k, runs Betweenness, and returns the bc slice
// plus the NodeIDs of the two bridge endpoints (k-1 and k).
func buildBridgeGraph(tb testing.TB, k int) (bc []float64, bridgeEndpointA, bridgeEndpointB uint64) {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	// Clique A: all edges among 0..k-1.
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				tb.Fatalf("AddEdge clique A (%d,%d): %v", i, j, err)
			}
		}
	}
	// Clique B: all edges among k..2k-1.
	for i := k; i < 2*k; i++ {
		for j := i + 1; j < 2*k; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				tb.Fatalf("AddEdge clique B (%d,%d): %v", i, j, err)
			}
		}
	}
	// Bridge: edge between k-1 (last of A) and k (first of B).
	if err := a.AddEdge(k-1, k, struct{}{}); err != nil {
		tb.Fatalf("AddEdge bridge (%d,%d): %v", k-1, k, err)
	}

	c := csr.BuildFromAdjList(a)
	bc = Betweenness(c)

	idA, okA := a.Mapper().Lookup(k - 1)
	idB, okB := a.Mapper().Lookup(k)
	if !okA || !okB {
		tb.Fatalf("bridge endpoint keys not found in mapper")
	}
	return bc, uint64(idA), uint64(idB)
}

// bridgeAssertTB is the minimal subset of testing.TB used by
// assertBridgeDominance. Both *testing.T and *rapid.T satisfy it,
// even across Go versions that extend testing.TB with new methods
// (e.g., ArtifactDir added in Go 1.26).
type bridgeAssertTB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// assertBridgeDominance is callable with both *testing.T and *rapid.T.
// It asserts:
//  1. bc(bridgeA) > 0 and bc(bridgeB) > 0 (bridge endpoints have
//     nonzero betweenness — some cross-cluster pair exists).
//  2. Both bridge endpoints strictly exceed every non-bridge vertex.
//  3. All bc values are non-negative and finite.
func assertBridgeDominance(tb bridgeAssertTB, bc []float64, bridgeA, bridgeB uint64) {
	tb.Helper()

	// All values must be finite and non-negative.
	for i, v := range bc {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			tb.Fatalf("bc[%d] = %f: not a finite non-negative value", i, v)
		}
	}

	bcA := bc[bridgeA]
	bcB := bc[bridgeB]

	if bcA <= 0 {
		tb.Fatalf("bridge endpoint A bc = %f, want > 0", bcA)
	}
	if bcB <= 0 {
		tb.Fatalf("bridge endpoint B bc = %f, want > 0", bcB)
	}

	// Collect all non-bridge bc values.
	bridgeSet := map[uint64]bool{bridgeA: true, bridgeB: true}
	for i, v := range bc {
		if bridgeSet[uint64(i)] {
			continue
		}
		if v >= bcA {
			tb.Fatalf("non-bridge node %d bc = %f >= bridge-endpoint-A bc = %f", i, v, bcA)
		}
		if v >= bcB {
			tb.Fatalf("non-bridge node %d bc = %f >= bridge-endpoint-B bc = %f", i, v, bcB)
		}
	}

	// Bridge endpoints must occupy the top-2 ranks.
	ranked := make([]float64, len(bc))
	copy(ranked, bc)
	sort.Float64s(ranked)
	n := len(ranked)
	top2 := map[float64]bool{ranked[n-1]: true, ranked[n-2]: true}
	if !top2[bcA] || !top2[bcB] {
		tb.Fatalf("bridge endpoints are not top-2 ranked: bcA=%f bcB=%f ranked top-2=%v", bcA, bcB, []float64{ranked[n-2], ranked[n-1]})
	}
}
