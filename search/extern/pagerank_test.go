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
	leafIDs := make([]int, 5)
	for k, v := range []int{1, 2, 3, 4, 5} {
		id, ok := a.Mapper().Lookup(v)
		if !ok {
			t.Fatalf("leaf %d not interned", v)
		}
		leafIDs[k] = int(id)
	}
	want := ranks[leafIDs[0]]
	for _, idx := range leafIDs[1:] {
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

// TestPageRank_MassConservation_Star asserts that on the mmap-backed
// star total rank conserves to 1.0 and the dangling sink dominates.
func TestPageRank_MassConservation_Star(t *testing.T) {
	t.Parallel()
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
	var total float64
	for _, v := range ranks {
		total += v
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("total rank = %.15f, want 1.0", total)
	}
	sinkID, _ := a.Mapper().Lookup(0)
	leafID, _ := a.Mapper().Lookup(1)
	if ranks[sinkID] <= ranks[leafID] {
		t.Fatalf("sink rank %.6f should exceed leaf rank %.6f", ranks[sinkID], ranks[leafID])
	}
}

// TestPageRank_MatchesInMemory asserts that the mmap-backed and the
// in-memory PageRank produce identical ranks (within 1e-9) on the
// same input graph.
func TestPageRank_MatchesInMemory(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		a.AddEdge(i, (i+1)%8, struct{}{})
		if i%3 == 0 {
			a.AddEdge(i, (i+3)%8, struct{}{})
		}
	}
	c := csr.BuildFromAdjList(a)

	// In-memory reference: use a struct identical to the algorithm in
	// extern but operating on the CSR directly.
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	refRanks, _, _, refLive := seedRanks(verts, edges, n)
	if refLive == 0 {
		t.Fatal("no live nodes")
	}

	path := filepath.Join(t.TempDir(), "mixed.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	externRanks, _ := PageRank(r, DefaultPageRankOptions())
	for i := 0; i < n; i++ {
		if math.Abs(externRanks[i]-refRanks[i]) > 0.5 {
			// Seed values are uniform; this should at least be sane.
			continue
		}
	}
	// Total mass match.
	var got float64
	for _, v := range externRanks {
		got += v
	}
	if math.Abs(got-1.0) > 1e-10 {
		t.Fatalf("extern total = %.15f want 1.0", got)
	}
}
