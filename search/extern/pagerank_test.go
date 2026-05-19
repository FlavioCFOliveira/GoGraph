package extern

import (
	"math"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/store/csrfile"
)

func TestPageRank_Star(t *testing.T) {
	t.Parallel()
	// All 5 leaves point at node 0; node 0 has no out-edges.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 1; i <= 5; i++ {
		a.AddEdge(i, 0, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "star.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	ranks, _ := PageRank(r, DefaultPageRankOptions())
	if len(ranks) == 0 {
		t.Fatalf("empty ranks")
	}
	// The five leaves should have equal rank.
	want := ranks[1]
	for _, idx := range []int{2, 3, 4, 5} {
		if math.Abs(ranks[idx]-want) > 1e-9 {
			t.Fatalf("rank[%d] = %v, want %v", idx, ranks[idx], want)
		}
	}
}

func TestPageRank_EmptyGraph(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "empty.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ranks, iters := PageRank(r, DefaultPageRankOptions())
	if len(ranks) != 0 || iters != 0 {
		t.Fatalf("empty graph: ranks=%d iters=%d", len(ranks), iters)
	}
}
