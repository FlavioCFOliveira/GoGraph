// Package search provides graph traversal and path-finding algorithms
// over the immutable [gograph/graph/csr.CSR] read-only view.
//
// All algorithms operate in [graph.NodeID] space; callers wishing to
// work with the user-facing N values resolve them via the originating
// [graph.Mapper]. This keeps the hot path allocation-free and free of
// interface dispatch in the inner loops.
//
// # Concurrency
//
// The algorithms are read-only over their CSR input and are safe to
// invoke concurrently with each other on the same CSR. They are also
// safe to invoke concurrently with each other across different CSRs.
// They do not, on their own, hold any mutable state outside the local
// stack frame.
//
// # Allocation and pooling
//
// Working buffers (frontier queue, recursion stack, visited bitset)
// are pooled across calls via [sync.Pool], so a steady-state workload
// that traverses graphs of similar size reaches zero heap allocations
// per call after warm-up.
package search

import (
	"context"
	"sync"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// bfsState is the per-call working state of [BFS]. The wavefront
// design keeps memory bounded by the widest level instead of the
// whole reachable subgraph.
type bfsState struct {
	cur     []graph.NodeID
	next    []graph.NodeID
	visited []uint64
}

var bfsPool = sync.Pool{
	New: func() any { return &bfsState{} },
}

func acquireBFS(bitsetWords int) *bfsState {
	metrics.IncCounter("search.pool.bfs.get", 1)
	s, _ := bfsPool.Get().(*bfsState)
	if cap(s.visited) < bitsetWords {
		s.visited = make([]uint64, bitsetWords)
	} else {
		s.visited = s.visited[:bitsetWords]
		for i := range s.visited {
			s.visited[i] = 0
		}
	}
	s.cur = s.cur[:0]
	s.next = s.next[:0]
	return s
}

func releaseBFS(s *bfsState) { metrics.IncCounter("search.pool.bfs.put", 1); bfsPool.Put(s) }

// dfsState is the per-call working state of [DFS]. The stack holds
// (node, depth) pairs so the iterative form preserves the visitation
// order of the canonical recursive DFS.
type dfsState struct {
	stack   []frontierItem
	visited []uint64
}

type frontierItem struct {
	node  graph.NodeID
	depth int32
}

var dfsPool = sync.Pool{
	New: func() any { return &dfsState{} },
}

func acquireDFS(bitsetWords int) *dfsState {
	metrics.IncCounter("search.pool.dfs.get", 1)
	s, _ := dfsPool.Get().(*dfsState)
	if cap(s.visited) < bitsetWords {
		s.visited = make([]uint64, bitsetWords)
	} else {
		s.visited = s.visited[:bitsetWords]
		for i := range s.visited {
			s.visited[i] = 0
		}
	}
	s.stack = s.stack[:0]
	return s
}

func releaseDFS(s *dfsState) { metrics.IncCounter("search.pool.dfs.put", 1); dfsPool.Put(s) }

func setVisited(visited []uint64, id graph.NodeID) {
	visited[uint64(id)>>6] |= 1 << (uint64(id) & 63)
}

func isVisited(visited []uint64, id graph.NodeID) bool {
	return visited[uint64(id)>>6]&(1<<(uint64(id)&63)) != 0
}

func bitsetWordsFor(maxID graph.NodeID) int {
	return int((uint64(maxID) + 63) / 64)
}

// BFS performs breadth-first traversal of c starting at src. The visit
// callback is invoked for every reached node in non-decreasing depth
// order, with depth starting at 0 for src; returning false from visit
// terminates the traversal early. The traversal visits each node at
// most once.
//
// BFS is allocation-free on the hot path after the first call (working
// buffers are reused via [sync.Pool]). Memory consumption per call is
// bounded by O(maxFrontierWidth) instead of O(reachableSize) thanks
// to the wavefront design: the current level's nodes are consumed and
// the next level is built in a separate buffer; the two buffers swap
// per level.
func BFS[W any](c *csr.CSR[W], src graph.NodeID, visit func(node graph.NodeID, depth int) bool) {
	defer metrics.Time("search.BFS")()
	_ = BFSCtx(context.Background(), c, src, visit)
}

// BFSCtx is the context-aware variant of [BFS]. ctx.Err() is checked
// once per BFS level; on cancellation the traversal returns the
// wrapped ctx.Err() without invoking visit on the partially-explored
// frontier.
func BFSCtx[W any](ctx context.Context, c *csr.CSR[W], src graph.NodeID, visit func(node graph.NodeID, depth int) bool) error {
	defer metrics.Time("search.BFSCtx")()
	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) {
		return nil
	}
	maxID := c.MaxNodeID()
	st := acquireBFS(bitsetWordsFor(maxID))
	defer releaseBFS(st)

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	setVisited(st.visited, src)
	st.cur = append(st.cur, src)
	depth := 0
	for len(st.cur) > 0 {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.BFSCtx.errors", 1)
			return err
		}
		for _, node := range st.cur {
			if !visit(node, depth) {
				return nil
			}
			start := verts[uint64(node)]
			end := verts[uint64(node)+1]
			for k := start; k < end; k++ {
				nb := edges[k]
				if isVisited(st.visited, nb) {
					continue
				}
				setVisited(st.visited, nb)
				st.next = append(st.next, nb)
			}
		}
		st.cur, st.next = st.next, st.cur[:0]
		depth++
	}
	return nil
}

// DFS performs iterative depth-first traversal of c starting at src.
// The visit callback receives every reached node and its depth from
// src (depth=0 for src); returning false terminates the traversal
// early. The traversal is iterative (it uses an explicit stack) so
// graphs with arbitrarily long paths do not risk a Go goroutine stack
// overflow. Each node is visited at most once. Visitation order is
// deterministic for any given CSR (it follows the order in which
// out-neighbours appear in the underlying edges array).
//
// DFS is allocation-free on the hot path after the first call.
func DFS[W any](c *csr.CSR[W], src graph.NodeID, visit func(node graph.NodeID, depth int) bool) {
	defer metrics.Time("search.DFS")()
	_ = DFSCtx(context.Background(), c, src, visit)
}

// DFSCtx is the context-aware variant of [DFS]. ctx.Err() is checked
// every 4096 frame pops; on cancellation the traversal returns
// the wrapped ctx.Err() without further visit invocations.
func DFSCtx[W any](ctx context.Context, c *csr.CSR[W], src graph.NodeID, visit func(node graph.NodeID, depth int) bool) error {
	defer metrics.Time("search.DFSCtx")()
	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) {
		return nil
	}
	maxID := c.MaxNodeID()
	st := acquireDFS(bitsetWordsFor(maxID))
	defer releaseDFS(st)

	st.stack = append(st.stack, frontierItem{node: src, depth: 0})
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	popCount := 0
	for len(st.stack) > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.DFSCtx.errors", 1)
				return err
			}
		}
		popCount++
		top := st.stack[len(st.stack)-1]
		st.stack = st.stack[:len(st.stack)-1]
		if isVisited(st.visited, top.node) {
			continue
		}
		setVisited(st.visited, top.node)
		if !visit(top.node, int(top.depth)) {
			return nil
		}
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := end; k > start; k-- {
			nb := edges[k-1]
			if isVisited(st.visited, nb) {
				continue
			}
			st.stack = append(st.stack, frontierItem{node: nb, depth: top.depth + 1})
		}
	}
	return nil
}
