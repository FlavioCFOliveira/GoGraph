package centrality

import (
	"math"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestPageRank_Star(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 1; i <= 5; i++ {
		a.AddEdge(i, 0, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	ranks, _ := PageRank(c, DefaultPageRankOptions())
	want := ranks[1]
	for _, i := range []int{2, 3, 4, 5} {
		if math.Abs(ranks[i]-want) > 1e-9 {
			t.Fatalf("rank[%d] = %f, rank[1] = %f", i, ranks[i], want)
		}
	}
}

func TestPageRank_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	c := csr.BuildFromAdjList(a)
	ranks, iters := PageRank(c, DefaultPageRankOptions())
	if len(ranks) != 0 || iters != 0 {
		t.Fatalf("empty: ranks=%v iters=%d", ranks, iters)
	}
}
