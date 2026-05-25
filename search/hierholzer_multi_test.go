package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestHierholzerUndirected_Multigraph verifies that HierholzerUndirected
// handles parallel edges correctly. The graph is C3 (triangle 0-1-2)
// with every edge doubled, giving 6 undirected edges. All vertex
// degrees are 4 (even), so an Eulerian circuit must exist with
// length |E|+1 = 7.
func TestHierholzerUndirected_Multigraph(t *testing.T) {
	t.Parallel()
	// C3 with doubled edges:
	// 0-1 (×2), 1-2 (×2), 0-2 (×2)
	// Degrees: 0→4, 1→4, 2→4 — all even.
	a := adjlist.New[int, int64](adjlist.Config{
		Directed:   false,
		Multigraph: true,
	})
	for _, pair := range [3][2]int{{0, 1}, {1, 2}, {0, 2}} {
		for rep := 0; rep < 2; rep++ {
			if err := a.AddEdge(pair[0], pair[1], int64(1)); err != nil {
				t.Fatalf("AddEdge(%d,%d) rep%d: %v", pair[0], pair[1], rep, err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	circuit, err := HierholzerUndirected(c)
	if err != nil {
		t.Fatalf("HierholzerUndirected: %v", err)
	}
	// |E|+1 = 6+1 = 7
	if len(circuit) != 7 {
		t.Fatalf("circuit length = %d, want 7 (6 edges + 1)", len(circuit))
	}
	if circuit[0] != circuit[6] {
		t.Fatalf("circuit must close: circuit[0]=%d circuit[6]=%d", circuit[0], circuit[6])
	}

	// Verify the circuit traverses exactly 6 directed hops (each
	// undirected edge consumed exactly once, counting multiplicity).
	// Count occurrences of each normalised pair in the trail; each
	// pair (0-1, 1-2, 0-2) should appear exactly twice.
	type key struct{ lo, hi int }
	counts := make(map[key]int)
	mapper := a.Mapper()
	for i := 0; i+1 < len(circuit); i++ {
		uID, vID := circuit[i], circuit[i+1]
		uLabel, ok1 := mapper.Resolve(uID)
		vLabel, ok2 := mapper.Resolve(vID)
		if !ok1 || !ok2 {
			t.Fatalf("Resolve failed for circuit step %d", i)
		}
		lo, hi := uLabel, vLabel
		if lo > hi {
			lo, hi = hi, lo
		}
		counts[key{lo, hi}]++
	}
	want := map[key]int{{0, 1}: 2, {1, 2}: 2, {0, 2}: 2}
	for k, wantCount := range want {
		if counts[k] != wantCount {
			t.Errorf("edge (%d,%d): appeared %d times, want %d", k.lo, k.hi, counts[k], wantCount)
		}
	}
}
