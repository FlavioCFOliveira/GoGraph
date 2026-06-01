package search

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestTarjanSCC_SingleNode(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddNode(0); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	sccs := TarjanSCC(c)
	if len(sccs) != 0 {
		t.Fatalf("isolated node with no edges should not appear in SCC output, got %d", len(sccs))
	}
}

func TestTarjanSCC_TwoCycles(t *testing.T) {
	t.Parallel()
	// Two SCCs: {0,1,2} and {3,4}; plus edge 2->3 connecting them.
	edges := [][2]int{
		{0, 1}, {1, 2}, {2, 0},
		{3, 4}, {4, 3},
		{2, 3},
	}
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	sccs := TarjanSCC(c)

	// Translate back to int for comparison.
	got := make([][]int, 0, len(sccs))
	for _, comp := range sccs {
		ids := make([]int, 0, len(comp))
		for _, n := range comp {
			v, _ := a.Mapper().Resolve(n)
			ids = append(ids, v)
		}
		sort.Ints(ids)
		got = append(got, ids)
	}
	sort.Slice(got, func(i, j int) bool { return got[i][0] < got[j][0] })

	want := [][]int{{0, 1, 2}, {3, 4}}
	if len(got) != len(want) {
		t.Fatalf("SCC count = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("SCC %d size = %d, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("SCC %d mismatch: got=%v want=%v", i, got[i], want[i])
			}
		}
	}
}

func TestTarjanSCC_NoCycles(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	sccs := TarjanSCC(c)
	for _, comp := range sccs {
		if len(comp) != 1 {
			t.Fatalf("DAG SCC sizes must all be 1, got %d", len(comp))
		}
	}
}
