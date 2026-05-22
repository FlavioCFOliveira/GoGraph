package adjlist

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

func collectNeighbours[N comparable, W any](a *AdjList[N, W], src N) []N {
	var out []N
	for v := range a.Neighbours(src) {
		out = append(out, v)
	}
	return out
}

func TestAdjList_DirectedSimple(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})

	mustAddEdge(t, a, "a", "b", 1)
	mustAddEdge(t, a, "a", "c", 2)
	mustAddEdge(t, a, "b", "c", 3)

	if got := a.Order(); got != 3 {
		t.Fatalf("Order = %d, want 3", got)
	}
	if got := a.Size(); got != 3 {
		t.Fatalf("Size = %d, want 3", got)
	}
	if !a.HasEdge("a", "b") || !a.HasEdge("a", "c") || !a.HasEdge("b", "c") {
		t.Fatalf("expected all three directed edges present")
	}
	if a.HasEdge("b", "a") {
		t.Fatalf("directed graph: reverse edge b->a should not exist")
	}

	// Duplicate insert is idempotent in simple-graph mode.
	mustAddEdge(t, a, "a", "b", 999)
	if got := a.Size(); got != 3 {
		t.Fatalf("duplicate insert changed Size: %d", got)
	}
}

func TestAdjList_UndirectedMirror(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false})
	mustAddEdge(t, a, "a", "b", 1)
	if !a.HasEdge("a", "b") || !a.HasEdge("b", "a") {
		t.Fatalf("undirected: both a->b and b->a must exist")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 (undirected edge counted once)", got)
	}
}

func TestAdjList_Multigraph(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	mustAddEdge(t, a, "a", "b", 1)
	mustAddEdge(t, a, "a", "b", 2)
	mustAddEdge(t, a, "a", "b", 3)
	if got := a.Size(); got != 3 {
		t.Fatalf("Size = %d, want 3 (parallel edges)", got)
	}
	nb := collectNeighbours(a, "a")
	if len(nb) != 3 {
		t.Fatalf("Neighbours len = %d, want 3", len(nb))
	}
	for _, n := range nb {
		if n != "b" {
			t.Fatalf("unexpected neighbour: %q", n)
		}
	}
}

func TestAdjList_SelfLoopDirected(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddEdge(t, a, "a", "a", 1)
	if !a.HasEdge("a", "a") {
		t.Fatalf("self-loop must be present")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
}

func TestAdjList_SelfLoopUndirected(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false})
	mustAddEdge(t, a, "a", "a", 1)
	if !a.HasEdge("a", "a") {
		t.Fatalf("self-loop must be present")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 (self-loop not double-counted in undirected)", got)
	}
}

func TestAdjList_RemoveEdge(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddEdge(t, a, "a", "b", 1)
	mustAddEdge(t, a, "a", "c", 2)

	a.RemoveEdge("a", "b")
	if a.HasEdge("a", "b") {
		t.Fatalf("a->b should be gone after RemoveEdge")
	}
	if !a.HasEdge("a", "c") {
		t.Fatalf("a->c must remain after RemoveEdge(a,b)")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}

	// Re-adding a tombstoned edge resurrects it.
	mustAddEdge(t, a, "a", "b", 5)
	if !a.HasEdge("a", "b") {
		t.Fatalf("a->b should be restored after re-add")
	}
	if got := a.Size(); got != 2 {
		t.Fatalf("Size = %d, want 2", got)
	}
}

func TestAdjList_RemoveEdge_UndirectedMirrored(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false})
	mustAddEdge(t, a, "a", "b", 1)
	a.RemoveEdge("a", "b")
	if a.HasEdge("a", "b") || a.HasEdge("b", "a") {
		t.Fatalf("undirected RemoveEdge must remove both directions")
	}
	if got := a.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
}

func TestAdjList_RemoveEdge_Unknown(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddEdge(t, a, "a", "b", 1)
	a.RemoveEdge("a", "z") // unknown dst
	a.RemoveEdge("z", "a") // unknown src
	a.RemoveEdge("a", "a") // never inserted self-loop
	if !a.HasEdge("a", "b") {
		t.Fatalf("unrelated RemoveEdge calls must not affect existing edges")
	}
}

func TestAdjList_Compact(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	for i := 0; i < 8; i++ {
		mustAddEdge(t, a, "a", "b", i)
	}
	// Simple graph collapses duplicates → only one edge a->b.
	mustAddEdge(t, a, "a", "c", 100)
	a.RemoveEdge("a", "b")
	a.Compact()
	if a.HasEdge("a", "b") {
		t.Fatalf("removed edge must remain removed after Compact")
	}
	if !a.HasEdge("a", "c") {
		t.Fatalf("Compact removed an unrelated edge")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
}

func TestAdjList_Neighbours_UnknownSrc(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	got := collectNeighbours(a, "ghost")
	if len(got) != 0 {
		t.Fatalf("Neighbours of unknown src must be empty, got %v", got)
	}
}

func TestAdjList_Neighbours_EarlyStop(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true})
	for i := 1; i <= 100; i++ {
		mustAddEdge(t, a, 0, i, i)
	}
	visited := 0
	for range a.Neighbours(0) {
		visited++
		if visited == 5 {
			break
		}
	}
	if visited != 5 {
		t.Fatalf("early break did not stop iteration: visited=%d", visited)
	}
}

func TestAdjList_AddNodeDoesNotCreateEdges(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddNode(t, a, "solitary")
	if got := a.Order(); got != 1 {
		t.Fatalf("Order = %d, want 1", got)
	}
	if got := a.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	if len(collectNeighbours(a, "solitary")) != 0 {
		t.Fatalf("solitary node has no edges")
	}
}

func TestAdjList_Concurrent_WritersReaders(t *testing.T) {
	t.Parallel()
	const (
		writers      = 256
		readers      = 1024
		edgesEach    = 256
		readsEach    = 512
		nodeUniverse = 4096
	)
	a := New[int, int](Config{Directed: true, Multigraph: false})

	var wg sync.WaitGroup
	var writeErrors atomic.Int64

	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 1)) //nolint:gosec // deterministic test RNG
			for i := 0; i < edgesEach; i++ {
				src := r.IntN(nodeUniverse)
				dst := r.IntN(nodeUniverse)
				if err := a.AddEdge(src, dst, w*1000+i); err != nil {
					writeErrors.Add(1)
					return
				}
				if !a.HasEdge(src, dst) {
					writeErrors.Add(1)
					return
				}
			}
		}(w)
	}
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(r), 7)) //nolint:gosec // deterministic test RNG
			for i := 0; i < readsEach; i++ {
				src := rng.IntN(nodeUniverse)
				dst := rng.IntN(nodeUniverse)
				// Both possible answers are valid given concurrent writers;
				// the assertion is that the call must not panic or deadlock.
				_ = a.HasEdge(src, dst)
				// Also exercise the iterator on the same shard while
				// writers may be racing on it.
				n := 0
				for range a.Neighbours(src) {
					n++
					if n > 16 {
						break
					}
				}
			}
		}(r)
	}
	wg.Wait()
	if e := writeErrors.Load(); e != 0 {
		t.Fatalf("writers observed %d HasEdge inconsistencies after AddEdge", e)
	}
	if a.Order() == 0 {
		t.Fatalf("no nodes were inserted")
	}
	if a.Size() == 0 {
		t.Fatalf("no edges were inserted")
	}
}

func BenchmarkAdjList_AddEdge_Million(b *testing.B) {
	a := New[uint32, struct{}](Config{Directed: true})
	const universe = 1 << 20 // 1M nodes
	// Pre-intern the node universe so AddEdge exercises the Mapper
	// fast path; the AC targets AddEdge on a graph that already has
	// 10^6 nodes, not the cold-load throughput.
	for i := 0; i < universe; i++ {
		mustAddNode(b, a, uint32(i))
	}
	src := make([]uint32, b.N)
	dst := make([]uint32, b.N)
	r := rand.New(rand.NewPCG(42, 1)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < b.N; i++ {
		src[i] = uint32(r.IntN(universe))
		dst[i] = uint32(r.IntN(universe))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustAddEdge(b, a, src[i], dst[i], struct{}{})
	}
}

// BenchmarkAdjList_HasEdge_HotCache measures HasEdge on a graph that
// fits comfortably in L2/L3 cache, isolating data-structure cost from
// the DRAM latency floor that dominates the million-node variant.
func BenchmarkAdjList_HasEdge_HotCache(b *testing.B) {
	a := New[uint32, struct{}](Config{Directed: true})
	const universe = 1 << 10 // 1024 nodes
	const fill = 1 << 13     // 8192 edges (avg degree 8)
	for i := 0; i < universe; i++ {
		mustAddNode(b, a, uint32(i))
	}
	r := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < fill; i++ {
		mustAddEdge(b, a, uint32(r.IntN(universe)), uint32(r.IntN(universe)), struct{}{})
	}
	probesS := make([]uint32, b.N)
	probesD := make([]uint32, b.N)
	for i := 0; i < b.N; i++ {
		probesS[i] = uint32(r.IntN(universe))
		probesD[i] = uint32(r.IntN(universe))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = a.HasEdge(probesS[i], probesD[i])
	}
}

// BenchmarkAdjList_AddEdge_HotCache measures AddEdge throughput on a
// small graph that stays resident in cache and grows monotonically,
// isolating data-structure cost from cache-miss noise.
func BenchmarkAdjList_AddEdge_HotCache(b *testing.B) {
	a := New[uint32, struct{}](Config{Directed: true, Multigraph: true})
	const universe = 1 << 10 // 1024 nodes
	for i := 0; i < universe; i++ {
		mustAddNode(b, a, uint32(i))
	}
	src := make([]uint32, b.N)
	dst := make([]uint32, b.N)
	r := rand.New(rand.NewPCG(13, 1)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < b.N; i++ {
		src[i] = uint32(r.IntN(universe))
		dst[i] = uint32(r.IntN(universe))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustAddEdge(b, a, src[i], dst[i], struct{}{})
	}
}

func BenchmarkAdjList_HasEdge_Million(b *testing.B) {
	a := New[uint32, struct{}](Config{Directed: true})
	const universe = 1 << 20
	const fill = 1 << 22 // 4M edges
	for i := 0; i < universe; i++ {
		mustAddNode(b, a, uint32(i))
	}
	r := rand.New(rand.NewPCG(11, 1)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < fill; i++ {
		mustAddEdge(b, a, uint32(r.IntN(universe)), uint32(r.IntN(universe)), struct{}{})
	}
	// Sample queries (mix of hit and miss).
	probesS := make([]uint32, b.N)
	probesD := make([]uint32, b.N)
	for i := 0; i < b.N; i++ {
		probesS[i] = uint32(r.IntN(universe))
		probesD[i] = uint32(r.IntN(universe))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = a.HasEdge(probesS[i], probesD[i])
	}
}
