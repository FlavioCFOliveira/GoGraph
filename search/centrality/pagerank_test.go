package centrality

import (
	"errors"
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
	ranks, _, _ := PageRank(c, DefaultPageRankOptions())
	// Resolve leaf NodeIDs and assert symmetric ranks.
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
			t.Fatalf("rank[%d] = %f, leaf-0 rank = %f", idx, ranks[idx], want)
		}
	}
}

func TestPageRank_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	c := csr.BuildFromAdjList(a)
	ranks, iters, _ := PageRank(c, DefaultPageRankOptions())
	if len(ranks) != 0 || iters != 0 {
		t.Fatalf("empty: ranks=%v iters=%d", ranks, iters)
	}
}

// TestPageRank_MassConservation_Star asserts that on a graph with a
// dangling sink the total rank conserves to 1.0 within numerical
// tolerance. This is the regression test for the v1.0.0 bug where
// dangling-node mass leaked each iteration and the sink lost almost
// all of its accumulated rank.
func TestPageRank_MassConservation_Star(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 1; i <= 5; i++ {
		a.AddEdge(i, 0, struct{}{}) // leaves -> sink
	}
	c := csr.BuildFromAdjList(a)
	ranks, _, _ := PageRank(c, DefaultPageRankOptions())
	var total float64
	for _, r := range ranks {
		total += r
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("total rank = %.15f, want 1.0 (delta %.3g)", total, math.Abs(total-1.0))
	}
	// Sink (dangling) must carry the dominant mass — it is the only
	// destination of every edge, so its stationary rank should be
	// strictly greater than any leaf's rank.
	sinkID, _ := a.Mapper().Lookup(0)
	leafID, _ := a.Mapper().Lookup(1)
	if ranks[sinkID] <= ranks[leafID] {
		t.Fatalf("sink rank %.6f should exceed leaf rank %.6f", ranks[sinkID], ranks[leafID])
	}
}

// TestPageRank_MassConservation_Cycle asserts that on a directed
// cycle every node receives the same rank 1/N and the total
// conserves to 1.0.
func TestPageRank_MassConservation_Cycle(t *testing.T) {
	t.Parallel()
	const k = 7
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < k; i++ {
		a.AddEdge(i, (i+1)%k, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	ranks, _, _ := PageRank(c, DefaultPageRankOptions())
	var total float64
	var liveCount int
	for _, r := range ranks {
		total += r
		if r > 0 {
			liveCount++
		}
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("total rank = %.15f, want 1.0", total)
	}
	if liveCount != k {
		t.Fatalf("live count = %d, want %d", liveCount, k)
	}
	// All nodes in the cycle are symmetric; their ranks must equal 1/k.
	const want = 1.0 / float64(k)
	for i := 0; i < k; i++ {
		id, _ := a.Mapper().Lookup(i)
		if math.Abs(ranks[id]-want) > 1e-6 {
			t.Fatalf("rank[%d] = %.10f, want %.10f", i, ranks[id], want)
		}
	}
}

// TestPageRank_MassConservation_Chain asserts that on a directed
// chain with a dangling tail node, total rank conserves to 1.0 and
// upstream nodes carry strictly less mass than downstream ones.
func TestPageRank_MassConservation_Chain(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(1, 2, struct{}{})
	a.AddEdge(2, 3, struct{}{})
	c := csr.BuildFromAdjList(a)
	ranks, _, _ := PageRank(c, DefaultPageRankOptions())
	var total float64
	for _, r := range ranks {
		total += r
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("total rank = %.15f, want 1.0", total)
	}
	id0, _ := a.Mapper().Lookup(0)
	id1, _ := a.Mapper().Lookup(1)
	id2, _ := a.Mapper().Lookup(2)
	id3, _ := a.Mapper().Lookup(3)
	if !(ranks[id3] > ranks[id2] && ranks[id2] > ranks[id1] && ranks[id1] > ranks[id0]) {
		t.Fatalf("chain rank ordering broken: r0=%.6f r1=%.6f r2=%.6f r3=%.6f",
			ranks[id0], ranks[id1], ranks[id2], ranks[id3])
	}
}

// TestPageRank_IsolatedGhostNodes asserts that ghost NodeID slots
// (created by sharded packing on small graphs) receive zero rank and
// do not inflate the live count. This pins the v1 contract that the
// rank slice indexes by NodeID but only carries mass on live IDs.
func TestPageRank_IsolatedGhostNodes(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(1, 0, struct{}{})
	c := csr.BuildFromAdjList(a)
	ranks, _, _ := PageRank(c, DefaultPageRankOptions())
	var total float64
	var nonZero int
	for _, r := range ranks {
		total += r
		if r > 0 {
			nonZero++
		}
	}
	if math.Abs(total-1.0) > 1e-10 {
		t.Fatalf("total rank = %.15f, want 1.0", total)
	}
	if nonZero != 2 {
		t.Fatalf("live count = %d, want 2 (ghost slots must remain zero)", nonZero)
	}
}

func BenchmarkPageRank_Cycle1K(b *testing.B) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	const k = 1024
	for i := 0; i < k; i++ {
		a.AddEdge(i, (i+1)%k, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = PageRank(c, DefaultPageRankOptions())
	}
}

func TestPageRank_RejectsNaN(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, struct{}{})
	c := csr.BuildFromAdjList(a)
	_, _, err := PageRank(c, PageRankOptions{Damping: math.NaN()})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
