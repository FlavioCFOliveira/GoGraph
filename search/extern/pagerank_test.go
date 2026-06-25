package extern

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

func TestPageRank_Star(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 1; i <= 5; i++ {
		if err := a.AddEdge(i, 0, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
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

	ranks, _, _ := PageRank(r, DefaultPageRankOptions())
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
	ranks, iters, _ := PageRank(r, DefaultPageRankOptions())
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
		if err := a.AddEdge(i, 0, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
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

	ranks, _, _ := PageRank(r, DefaultPageRankOptions())
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

// TestPageRank_MatchesInMemory asserts the mmap-backed extern PageRank produces
// ranks element-wise identical (within 1e-9) to the in-core reference
// centrality.PageRank on the same graph, with the same top-ranked node — and
// that the total mass is 1.0. (#1773: the prior version compared against the
// uniform seed array with a 0.5 tolerance and a dead `continue` loop, so a
// wrong extern ranking would have passed; it now compares against the real
// reference implementation.)
func TestPageRank_MatchesInMemory(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		if err := a.AddEdge(i, (i+1)%8, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if i%3 == 0 {
			if err := a.AddEdge(i, (i+3)%8, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)

	// In-core reference with parameters matching extern's defaults.
	coreRanks, _, err := centrality.PageRank(c, centrality.PageRankOptions{
		Damping: 0.85, MaxIterations: 100, Tolerance: 1e-6,
	})
	if err != nil {
		t.Fatalf("centrality.PageRank: %v", err)
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
	externRanks, _, err := PageRank(r, DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("extern PageRank: %v", err)
	}

	if len(externRanks) != len(coreRanks) {
		t.Fatalf("rank vector length: extern=%d core=%d", len(externRanks), len(coreRanks))
	}
	// Element-wise agreement within 1e-9 — the assertion the old test omitted.
	for i := range coreRanks {
		if d := math.Abs(externRanks[i] - coreRanks[i]); d > 1e-9 {
			t.Errorf("rank[%d]: extern=%.12f core=%.12f diff=%.2e (want < 1e-9)", i, externRanks[i], coreRanks[i], d)
		}
	}
	// Ranking ORDER must agree: the highest-ranked node is the same.
	if ext, core := argmaxRank(externRanks), argmaxRank(coreRanks); ext != core {
		t.Errorf("top-ranked node differs: extern=%d core=%d", ext, core)
	}
	// Total mass is a probability distribution.
	var total float64
	for _, v := range externRanks {
		total += v
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("extern total mass = %.15f, want 1.0", total)
	}
}

// argmaxRank returns the index of the maximum rank (lowest index on ties).
func argmaxRank(r []float64) int {
	best := 0
	for i := 1; i < len(r); i++ {
		if r[i] > r[best] {
			best = i
		}
	}
	return best
}
