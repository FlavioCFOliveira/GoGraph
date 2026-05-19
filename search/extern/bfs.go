// Package extern provides graph algorithms that operate directly on
// a Tier 2 (mmap-backed) [csrfile.Reader] without first materialising
// the CSR in memory. The algorithms are semi-external: vertex-sized
// auxiliary structures (visited bitsets, level frontiers) live in
// RAM while edge data is streamed from the mapped file.
package extern

import (
	"context"
	"slices"

	"gograph/graph"
	"gograph/internal/metrics"
	"gograph/store/csrfile"
)

// BFS performs breadth-first traversal of the graph captured by r,
// starting at src. visit is invoked for every reached node in
// non-decreasing depth order; returning false aborts the traversal.
//
// The implementation keeps the visited bitset and per-level
// frontiers in RAM and streams adjacency directly from the mmap
// region. The current frontier is sorted before expansion so that
// edge reads stay sequential, maximising the benefit of any
// MADV_SEQUENTIAL hint configured on the reader.
func BFS(r *csrfile.Reader, src graph.NodeID, visit func(node graph.NodeID, depth int) bool) error {
	defer metrics.Time("search.extern.BFS")()
	err := BFSCtx(context.Background(), r, src, visit)
	if err != nil {
		metrics.IncCounter("search.extern.BFS.errors", 1)
	}
	return err
}

// BFSCtx is the context-aware variant of [BFS]. ctx.Err() is checked
// once per BFS level; on cancellation returns the wrapped ctx.Err.
func BFSCtx(ctx context.Context, r *csrfile.Reader, src graph.NodeID, visit func(node graph.NodeID, depth int) bool) error {
	defer metrics.Time("search.extern.BFSCtx")()
	verts := r.Vertices()
	edges := r.Edges()
	if uint64(src)+1 >= uint64(len(verts)) {
		return nil
	}

	visited := newVisited(uint64(len(verts)) - 1)
	cur := []graph.NodeID{src}
	var next []graph.NodeID
	visited.set(src)

	depth := 0
	for len(cur) > 0 {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.extern.BFSCtx.errors", 1)
			return err
		}
		slices.Sort(cur)
		for _, node := range cur {
			if !visit(node, depth) {
				return nil
			}
			start := verts[uint64(node)]
			end := verts[uint64(node)+1]
			for k := start; k < end; k++ {
				nb := edges[k]
				if visited.get(nb) {
					continue
				}
				visited.set(nb)
				next = append(next, nb)
			}
		}
		cur, next = next, cur[:0]
		depth++
	}
	return nil
}

// visitedSet is a simple uint64-packed bitset; small, allocation-
// free in the hot path, and bounded by the vertex count.
type visitedSet struct {
	words []uint64
}

func newVisited(n uint64) *visitedSet {
	return &visitedSet{words: make([]uint64, (n+63)/64)}
}

func (v *visitedSet) set(id graph.NodeID) {
	v.words[uint64(id)>>6] |= 1 << (uint64(id) & 63)
}

func (v *visitedSet) get(id graph.NodeID) bool {
	idx := uint64(id) >> 6
	if idx >= uint64(len(v.words)) {
		return false
	}
	return v.words[idx]&(1<<(uint64(id)&63)) != 0
}
