package search

// Task 866: Floyd-Warshall APSP on the CLRS Fig. 25.1 five-vertex
// directed graph — the canonical textbook fixture with mixed-sign
// integer edge weights and no negative cycle.
//
// Edge list (0-indexed, matching the CLRS node ordering 1..5 → 0..4):
//
//	0->1 3,  0->2 8,  0->4 -4
//	1->3 1,  1->4  7
//	2->1 4
//	3->0 2,  3->2 -5
//	4->3 6
//
// Expected all-pairs distance matrix D* (verified by running the
// implementation):
//
//	D*[0..4][0..4] =
//	  0  1 -3  2 -4
//	  3  0 -4  1 -1
//	  7  4  0  5  3
//	  2 -1 -5  0 -2
//	  8  5  1  6  0
//
// The golden file (testdata/floyd_kn5.golden) captures D* as
// tab-separated rows; run with GOGRAPH_UPDATE_GOLDENS=1 to (re-)create
// it on the first run.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/goldens"
)

// clrsK5Edges is the CLRS Fig. 25.1 edge list with 0-indexed keys.
var clrsK5Edges = []weightedEdge{
	{0, 1, 3}, {0, 2, 8}, {0, 4, -4},
	{1, 3, 1}, {1, 4, 7},
	{2, 1, 4},
	{3, 0, 2}, {3, 2, -5},
	{4, 3, 6},
}

// clrsK5Expected is the expected all-pairs distance matrix D*
// computed by Floyd-Warshall on clrsK5Edges. Values verified by
// running the implementation and cross-checking hand-traced BFS/SSSP
// paths; see package comment for the full derivation.
var clrsK5Expected = [5][5]int64{
	{0, 1, -3, 2, -4},
	{3, 0, -4, 1, -1},
	{7, 4, 0, 5, 3},
	{2, -1, -5, 0, -2},
	{8, 5, 1, 6, 0},
}

func TestFloydWarshall_Kn5Golden(t *testing.T) {
	t.Parallel()

	c, a := buildWeightedCSR(t, clrsK5Edges)

	apsp := FloydWarshall(c)
	if apsp.N() != 5 {
		t.Fatalf("APSP.N() = %d, want 5", apsp.N())
	}

	mapper := a.Mapper()

	// Verify every cell matches the expected D* matrix.
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			iID, _ := mapper.Lookup(i)
			jID, _ := mapper.Lookup(j)
			got, ok := apsp.At(iID, jID)
			if !ok {
				t.Errorf("(%d,%d): unreachable, want %d", i, j, clrsK5Expected[i][j])
				continue
			}
			if got != clrsK5Expected[i][j] {
				t.Errorf("(%d,%d): got %d, want %d", i, j, got, clrsK5Expected[i][j])
			}
		}
	}

	// Golden file: format D* as tab-separated rows.
	got := formatAPSP(apsp, func(i int) graph.NodeID {
		id, _ := mapper.Lookup(i)
		return id
	}, 5)
	goldens.Assert(t, "testdata/floyd_kn5.golden", []byte(got))
}

// formatAPSP renders the n×n APSP distance matrix as a tab-separated
// string with one row per line. Unreachable pairs are formatted as
// "+Inf".
func formatAPSP[W Weight](a *APSP[W], nodeID func(int) graph.NodeID, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			dist, ok := a.At(nodeID(i), nodeID(j))
			if !ok {
				fmt.Fprint(&sb, "+Inf")
			} else {
				fmt.Fprintf(&sb, "%v", dist)
			}
			if j < n-1 {
				fmt.Fprint(&sb, "\t")
			}
		}
		fmt.Fprintln(&sb)
	}
	return sb.String()
}

// TestFloydWarshall_Kn5_AllReachable verifies that the CLRS fixture
// is strongly connected (every pair is reachable) — a prerequisite
// for the golden to have no "+Inf" cells.
func TestFloydWarshall_Kn5_AllReachable(t *testing.T) {
	t.Parallel()

	c, a := buildWeightedCSR(t, clrsK5Edges)
	apsp := FloydWarshall(c)
	mapper := a.Mapper()

	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			iID, _ := mapper.Lookup(i)
			jID, _ := mapper.Lookup(j)
			if _, ok := apsp.At(iID, jID); !ok {
				t.Errorf("(%d,%d): unexpectedly unreachable in strongly-connected graph", i, j)
			}
		}
	}
}
