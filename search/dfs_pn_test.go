package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// buildPathCSR builds a directed path P_n via shapegen and returns the CSR.
func buildPathCSR(tb testing.TB, n int) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	tb.Helper()
	g, err := shapegen.Path(n, true).Build(adjlist.Config{Directed: true})
	if err != nil {
		tb.Fatalf("shapegen.Path(%d): %v", n, err)
	}
	a := g.AdjList()
	return csr.BuildFromAdjList(a), a
}

// TestDFS_Pn_Short verifies that iterative DFS on directed P_n:
//   - visits exactly n nodes, and
//   - the depth of node i equals i (monotone discovery order from src=0).
func TestDFS_Pn_Short(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 10, 1000} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			c, a := buildPathCSR(t, n)

			src, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatal("node 0 not found in mapper")
			}

			visited := 0
			depthOK := true
			DFS(c, src, func(node graph.NodeID, depth int) bool {
				key, _ := a.Mapper().Resolve(node)
				if depth != key {
					depthOK = false
				}
				visited++
				return true
			})

			if visited != n {
				t.Errorf("visited %d nodes, want %d", visited, n)
			}
			if !depthOK {
				t.Error("depth[i] != i: traversal order is not monotone from src=0")
			}
		})
	}
}

// TestDFS_Pn_Soak verifies that iterative DFS on P_1_000_000 does not
// overflow the Go stack (regression guard for recursive implementations).
func TestDFS_Pn_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const n = 1_000_000
	c, a := buildPathCSR(t, n)

	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("node 0 not found in mapper")
	}

	visited := 0
	DFS(c, src, func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	if visited != n {
		t.Errorf("visited %d nodes, want %d", visited, n)
	}
}
