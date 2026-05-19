package search

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// buildFromEdges returns a CSR over a directed graph with int nodes
// and unweighted edges defined by the (src, dst) pairs in edges.
func buildFromEdges(edges [][2]int) (*csr.CSR[struct{}], *adjlist.AdjList[int, struct{}]) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		a.AddEdge(e[0], e[1], struct{}{})
	}
	return csr.BuildFromAdjList(a), a
}

func TestBFS_Linear(t *testing.T) {
	t.Parallel()
	const n = 10
	edges := make([][2]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	c, a := buildFromEdges(edges)
	m := a.Mapper()
	srcID, _ := m.Lookup(0)
	depths := map[int]int{}
	BFS(c, srcID, func(node graph.NodeID, d int) bool {
		v, _ := m.Resolve(node)
		depths[v] = d
		return true
	})
	for i := 0; i < n; i++ {
		if depths[i] != i {
			t.Fatalf("node %d depth = %d, want %d", i, depths[i], i)
		}
	}
}

func TestBFS_Tree(t *testing.T) {
	t.Parallel()
	// Tree:
	//        0
	//      / | \
	//     1  2  3
	//    /|     |
	//   4 5     6
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 4}, {1, 5}, {3, 6}}
	c, a := buildFromEdges(edges)
	srcID, _ := a.Mapper().Lookup(0)
	depths := map[int]int{}
	BFS(c, srcID, func(node graph.NodeID, d int) bool {
		v, _ := a.Mapper().Resolve(node)
		depths[v] = d
		return true
	})
	want := map[int]int{0: 0, 1: 1, 2: 1, 3: 1, 4: 2, 5: 2, 6: 2}
	if len(depths) != len(want) {
		t.Fatalf("visited count = %d, want %d", len(depths), len(want))
	}
	for k, v := range want {
		if depths[k] != v {
			t.Fatalf("node %d depth = %d, want %d", k, depths[k], v)
		}
	}
}

func TestBFS_Disconnected_StopsAtComponent(t *testing.T) {
	t.Parallel()
	edges := [][2]int{{0, 1}, {1, 2}, {3, 4}}
	c, a := buildFromEdges(edges)
	srcID, _ := a.Mapper().Lookup(0)
	visited := map[int]bool{}
	BFS(c, srcID, func(node graph.NodeID, _ int) bool {
		v, _ := a.Mapper().Resolve(node)
		visited[v] = true
		return true
	})
	for _, want := range []int{0, 1, 2} {
		if !visited[want] {
			t.Fatalf("node %d should be reached", want)
		}
	}
	for _, leak := range []int{3, 4} {
		if visited[leak] {
			t.Fatalf("node %d unreachable from 0; must not be visited", leak)
		}
	}
}

func TestBFS_EarlyStop(t *testing.T) {
	t.Parallel()
	const n = 100
	edges := make([][2]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	c, a := buildFromEdges(edges)
	srcID, _ := a.Mapper().Lookup(0)
	count := 0
	BFS(c, srcID, func(_ graph.NodeID, _ int) bool {
		count++
		return count < 5
	})
	if count != 5 {
		t.Fatalf("visit calls = %d, want 5", count)
	}
}

func TestBFS_UnknownSrc(t *testing.T) {
	t.Parallel()
	c, _ := buildFromEdges([][2]int{{0, 1}})
	visited := 0
	BFS(c, graph.NodeID(1<<30), func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	if visited != 0 {
		t.Fatalf("BFS from unknown src visited %d nodes, want 0", visited)
	}
}

func TestDFS_DepthOrderOnTree(t *testing.T) {
	t.Parallel()
	// Tree:
	//   0
	//  / \
	// 1   2
	//      \
	//       3
	edges := [][2]int{{0, 1}, {0, 2}, {2, 3}}
	c, a := buildFromEdges(edges)
	srcID, _ := a.Mapper().Lookup(0)
	var order []int
	DFS(c, srcID, func(node graph.NodeID, _ int) bool {
		v, _ := a.Mapper().Resolve(node)
		order = append(order, v)
		return true
	})
	if len(order) != 4 {
		t.Fatalf("DFS visited %d nodes, want 4", len(order))
	}
	if order[0] != 0 {
		t.Fatalf("DFS first node = %d, want 0", order[0])
	}
	// The CSR sorts neighbours by source; within a source, the order
	// of neighbours is insertion-order. For our fixture, node 0's
	// neighbours are inserted 1 then 2, so DFS visits 0 -> 1 -> 2 -> 3.
	want := []int{0, 1, 2, 3}
	if !equalInts(order, want) {
		t.Fatalf("DFS order = %v, want %v", order, want)
	}
}

func TestDFS_VisitsEachOnce(t *testing.T) {
	t.Parallel()
	// Diamond with a back-edge: 0 -> 1 -> 2 -> 0 (cycle).
	edges := [][2]int{{0, 1}, {0, 2}, {1, 2}, {2, 0}}
	c, a := buildFromEdges(edges)
	srcID, _ := a.Mapper().Lookup(0)
	counts := map[int]int{}
	DFS(c, srcID, func(node graph.NodeID, _ int) bool {
		v, _ := a.Mapper().Resolve(node)
		counts[v]++
		return true
	})
	for k, v := range counts {
		if v != 1 {
			t.Fatalf("node %d visited %d times, want 1", k, v)
		}
	}
}

// TestBFS_ReachabilityRandomised verifies that BFS reaches exactly the
// nodes that are graph-theoretically reachable from src. The oracle is
// a naive transitive-closure computed independently from the same
// edge list.
func TestBFS_ReachabilityRandomised(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // deterministic test RNG
		const n = 64
		const e = 256
		edges := make([][2]int, 0, e)
		for i := 0; i < e; i++ {
			edges = append(edges, [2]int{r.IntN(n), r.IntN(n)})
		}
		c, a := buildFromEdges(edges)
		src := r.IntN(n)
		srcID, _ := a.Mapper().Lookup(src)
		if !idValid(srcID, c) {
			continue
		}
		// Oracle: BFS over an adjacency map.
		adj := map[int][]int{}
		for _, ed := range edges {
			adj[ed[0]] = append(adj[ed[0]], ed[1])
		}
		oracle := map[int]bool{src: true}
		queue := []int{src}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			for _, y := range adj[x] {
				if !oracle[y] {
					oracle[y] = true
					queue = append(queue, y)
				}
			}
		}
		got := map[int]bool{}
		BFS(c, srcID, func(node graph.NodeID, _ int) bool {
			v, _ := a.Mapper().Resolve(node)
			got[v] = true
			return true
		})
		for k := range oracle {
			if !got[k] {
				t.Fatalf("seed=%d src=%d: oracle reaches %d, BFS did not", seed, src, k)
			}
		}
		for k := range got {
			if !oracle[k] {
				t.Fatalf("seed=%d src=%d: BFS reached %d but oracle did not", seed, src, k)
			}
		}
	}
}

func TestBFS_DFS_Concurrent(t *testing.T) {
	t.Parallel()
	c, a := buildFromEdges(func() [][2]int {
		r := rand.New(rand.NewPCG(99, 1)) //nolint:gosec // deterministic test RNG
		const n = 256
		const e = 1024
		out := make([][2]int, 0, e)
		for i := 0; i < e; i++ {
			out = append(out, [2]int{r.IntN(n), r.IntN(n)})
		}
		return out
	}())
	const workers = 32
	const perWorker = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(seed), 13)) //nolint:gosec // deterministic test RNG
			for i := 0; i < perWorker; i++ {
				src := rng.IntN(256)
				srcID, ok := a.Mapper().Lookup(src)
				if !ok {
					continue
				}
				BFS(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
				DFS(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
			}
		}(w)
	}
	wg.Wait()
}

func idValid[W any](id graph.NodeID, c *csr.CSR[W]) bool {
	return uint64(id)+1 < uint64(len(c.VerticesSlice()))
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sanity import: silence unused warning if test set shrinks.
var _ = sort.Ints

func BenchmarkBFS_Chain10M(b *testing.B) {
	const n = 10_000_000
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
	for i := uint32(0); i < uint32(n-1); i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BFS(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
	}
}

func BenchmarkDFS_Chain10M(b *testing.B) {
	const n = 10_000_000
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
	for i := uint32(0); i < uint32(n-1); i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DFS(c, srcID, func(_ graph.NodeID, _ int) bool { return true })
	}
}

// TestBFSCtx_HonoursCancel ensures BFSCtx surfaces ctx.Err on a
// cancelled context.
func TestBFSCtx_HonoursCancel(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 100; i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := BFSCtx(ctx, c, src, func(_ graph.NodeID, _ int) bool { return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BFSCtx err=%v want context.Canceled", err)
	}
}
