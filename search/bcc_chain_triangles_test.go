package search

// Task 783: BCC on a chain of K=5 triangles sharing single vertices.
//
// Structure: triangles share one vertex with the next triangle, so the
// shared vertex is an articulation point. The chain has 11 nodes (0..10)
// and 15 edges (3 per triangle).
//
//   Triangle 0: {0, 1, 2}  — edges 0-1, 1-2, 0-2
//   Triangle 1: {2, 3, 4}  — edges 2-3, 3-4, 2-4   (shares vertex 2)
//   Triangle 2: {4, 5, 6}  — edges 4-5, 5-6, 4-6   (shares vertex 4)
//   Triangle 3: {6, 7, 8}  — edges 6-7, 7-8, 6-8   (shares vertex 6)
//   Triangle 4: {8, 9, 10} — edges 8-9, 9-10, 8-10  (shares vertex 8)
//
// Each triangle is its own biconnected component; the shared vertices are
// articulation points. No edge is a bridge (every edge belongs to a cycle).
//
// Expected:
//   - 5 biconnected components
//   - 0 bridges
//   - 4 articulation points: {2, 4, 6, 8}

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestHopcroftTarjanBCC_ChainTriangles tests BCC on a chain of 5 triangles.
func TestHopcroftTarjanBCC_ChainTriangles(t *testing.T) {
	t.Parallel()

	const k = 5 // number of triangles

	a := adjlist.New[int, int64](adjlist.Config{Directed: false})

	// Build k triangles. Triangle i uses vertices {2i, 2i+1, 2i+2}.
	// The last triangle uses vertex 2*(k-1) as the shared entry point.
	for i := 0; i < k; i++ {
		v0 := 2 * i
		v1 := 2*i + 1
		v2 := 2*i + 2
		for _, e := range [3][2]int{{v0, v1}, {v1, v2}, {v0, v2}} {
			if err := a.AddEdge(e[0], e[1], 0); err != nil {
				t.Fatalf("AddEdge(%d-%d): %v", e[0], e[1], err)
			}
		}
	}

	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	if len(res.Components) != k {
		t.Errorf("Components: got %d, want %d", len(res.Components), k)
	}
	if len(res.Bridges) != 0 {
		t.Errorf("Bridges: got %d, want 0 (got %v)", len(res.Bridges), res.Bridges)
	}

	// Articulation points are the (k-1) shared vertices {2, 4, 6, 8}.
	wantArtic := make([]int, k-1)
	for i := range wantArtic {
		wantArtic[i] = 2 * (i + 1)
	}
	gotArtic := resolveNodeIDs(a.Mapper(), res.Articulation)
	sort.Ints(gotArtic)
	if !intsEqual(gotArtic, wantArtic) {
		t.Errorf("Articulation: got %v, want %v", gotArtic, wantArtic)
	}
}
