package csr

import (
	"math/rand/v2"
	"sync"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
)

func TestCSR_BuildFromAdjList_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	c := BuildFromAdjList(a)
	if c.Order() != 0 || c.Size() != 0 {
		t.Fatalf("empty CSR: Order=%d Size=%d, want 0/0", c.Order(), c.Size())
	}
	if got := len(c.VerticesSlice()); got != 1 {
		t.Fatalf("empty CSR vertices length = %d, want 1", got)
	}
}

func TestCSR_BuildFromAdjList_Directed(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", 1)
	a.AddEdge("a", "c", 2)
	a.AddEdge("b", "c", 3)
	a.AddEdge("c", "a", 4)

	c := BuildFromAdjList(a)
	if c.Order() != 3 {
		t.Fatalf("Order = %d, want 3", c.Order())
	}
	if c.Size() != 4 {
		t.Fatalf("Size = %d, want 4", c.Size())
	}

	m := a.Mapper()
	idA, _ := m.Lookup("a")
	idB, _ := m.Lookup("b")
	idC, _ := m.Lookup("c")

	got := collect(c.NeighboursByID(idA))
	wantSet := map[graph.NodeID]int{idB: 1, idC: 2}
	for _, p := range got {
		if w, ok := wantSet[p.id]; !ok || w != p.w {
			t.Fatalf("Neighbours(a): unexpected pair %+v", p)
		}
		delete(wantSet, p.id)
	}
	if len(wantSet) != 0 {
		t.Fatalf("Neighbours(a) missing: %v", wantSet)
	}

	// Sanity check on b and c.
	if len(collect(c.NeighboursByID(idB))) != 1 {
		t.Fatalf("Neighbours(b) length != 1")
	}
	if len(collect(c.NeighboursByID(idC))) != 1 {
		t.Fatalf("Neighbours(c) length != 1")
	}
}

func TestCSR_RangeBeyondMax(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", struct{}{})
	c := BuildFromAdjList(a)
	out := collect(c.NeighboursByID(graph.NodeID(1 << 30)))
	if len(out) != 0 {
		t.Fatalf("Neighbours(huge) must yield nothing, got %d", len(out))
	}
}

func TestCSR_Unweighted_NoWeightsSlice(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", struct{}{})
	c := BuildFromAdjList(a)
	if c.WeightsSlice() != nil {
		t.Fatalf("CSR[struct{}] should not allocate a weights slice")
	}
}

func TestCSR_AdjListParityRandomised(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int](adjlist.Config{Directed: true, Multigraph: true})
	r := rand.New(rand.NewPCG(99, 17)) //nolint:gosec // deterministic test RNG
	const universe = 256
	const edges = 4096
	for i := 0; i < edges; i++ {
		a.AddEdge(r.IntN(universe), r.IntN(universe), i)
	}

	c := BuildFromAdjList(a)

	// For every source NodeID assigned, the multiset of (neighbour,
	// weight) pairs returned by the CSR must equal the multiset
	// returned by the AdjList.
	m := a.Mapper()
	for id := uint64(0); id < uint64(a.MaxNodeID()); id++ {
		nbA, wsA := a.LoadEntry(graph.NodeID(id))
		if len(nbA) == 0 {
			continue
		}
		setExpected := make(map[pair]int)
		for i, n := range nbA {
			setExpected[pair{n, wsA[i]}]++
		}
		setActual := make(map[pair]int)
		for n, w := range c.NeighboursByID(graph.NodeID(id)) {
			setActual[pair{n, w}]++
		}
		if !sameMultiset(setExpected, setActual) {
			t.Fatalf("source %d: CSR/AdjList multisets differ\n want %v\n  got %v",
				id, setExpected, setActual)
		}
	}
	_ = m
}

func TestCSR_ConcurrentReaders(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic test RNG
	const universe = 512
	for i := 0; i < 4096; i++ {
		a.AddEdge(r.IntN(universe), r.IntN(universe), i)
	}
	c := BuildFromAdjList(a)

	const readers = 1024
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(seed), 11)) //nolint:gosec // deterministic test RNG
			for j := 0; j < 256; j++ {
				id := graph.NodeID(uint64(rng.IntN(int(uint64(a.MaxNodeID())))))
				n := 0
				for range c.NeighboursByID(id) {
					n++
					if n > 64 {
						break
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

type pair struct {
	id graph.NodeID
	w  int
}

func sameMultiset(a, b map[pair]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

type nw[W any] struct {
	id graph.NodeID
	w  W
}

func collect[W any](seq func(yield func(graph.NodeID, W) bool)) []nw[W] {
	var out []nw[W]
	for id, w := range seq {
		out = append(out, nw[W]{id, w})
	}
	return out
}

func BenchmarkCSR_NeighboursByID(b *testing.B) {
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
	const universe = 1 << 20
	for i := 0; i < universe; i++ {
		a.AddNode(uint32(i))
	}
	r := rand.New(rand.NewPCG(31, 1)) //nolint:gosec // deterministic benchmark RNG
	const fill = 1 << 22
	for i := 0; i < fill; i++ {
		a.AddEdge(uint32(r.IntN(universe)), uint32(r.IntN(universe)), struct{}{})
	}
	c := BuildFromAdjList(a)

	probes := make([]graph.NodeID, b.N)
	maxID := uint64(a.MaxNodeID())
	for i := 0; i < b.N; i++ {
		probes[i] = graph.NodeID(uint64(r.IntN(int(maxID))))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for range c.NeighboursByID(probes[i]) {
			count++
		}
		_ = count
	}
}

func BenchmarkCSR_Build_TenMillion(b *testing.B) {
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
		const universe = 1 << 20
		for i := 0; i < universe; i++ {
			a.AddNode(uint32(i))
		}
		r := rand.New(rand.NewPCG(uint64(n), 1)) //nolint:gosec // deterministic benchmark RNG
		const fill = 10_000_000
		for i := 0; i < fill; i++ {
			a.AddEdge(uint32(r.IntN(universe)), uint32(r.IntN(universe)), struct{}{})
		}
		b.StartTimer()
		_ = BuildFromAdjList(a)
	}
}

func TestCSR_LiveMask_LiveNodes_LiveCount(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(1, 2, struct{}{})
	a.AddEdge(2, 3, struct{}{})
	a.AddEdge(3, 1, struct{}{})
	c := BuildFromAdjList(a)

	mask := c.LiveMask()
	if mask == nil {
		t.Fatal("LiveMask returned nil for non-empty CSR")
	}
	ids := c.LiveNodes()
	if got := c.LiveCount(); got != 3 {
		t.Fatalf("LiveCount = %d, want 3", got)
	}
	if len(ids) != 3 {
		t.Fatalf("len(LiveNodes) = %d, want 3", len(ids))
	}
	for _, id := range ids {
		if !mask[id] {
			t.Fatalf("LiveNodes returned %d but mask says not live", id)
		}
	}
	// Sorted property.
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("LiveNodes not sorted: %v", ids)
		}
	}
	// Total mask trues equals LiveCount.
	var liveTrue int
	for _, m := range mask {
		if m {
			liveTrue++
		}
	}
	if liveTrue != c.LiveCount() {
		t.Fatalf("mask trues = %d, LiveCount = %d", liveTrue, c.LiveCount())
	}
}

func TestCSR_LiveMask_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	c := BuildFromAdjList(a)
	if mask := c.LiveMask(); mask != nil {
		t.Fatalf("LiveMask on empty CSR = %v, want nil", mask)
	}
	if got := c.LiveCount(); got != 0 {
		t.Fatalf("LiveCount = %d, want 0", got)
	}
	if ids := c.LiveNodes(); ids != nil {
		t.Fatalf("LiveNodes on empty CSR = %v, want nil", ids)
	}
}

func TestCSR_LiveMask_DanglingSink(t *testing.T) {
	t.Parallel()
	// Sink node (only destination) must be flagged as live.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(1, 0, struct{}{})
	c := BuildFromAdjList(a)
	mask := c.LiveMask()
	id0, _ := a.Mapper().Lookup(0)
	id1, _ := a.Mapper().Lookup(1)
	if !mask[id0] || !mask[id1] {
		t.Fatalf("sink %d or source %d not flagged live: mask=%v", id0, id1, mask)
	}
	if got := c.LiveCount(); got != 2 {
		t.Fatalf("LiveCount = %d, want 2", got)
	}
}
