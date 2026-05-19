package search

import (
	"context"
	"sort"

	"gograph/graph"
	"gograph/graph/csr"
)

// YenPath is one shortest path produced by [YenKShortest].
//
// Concurrency: YenPath values are freshly allocated per call and
// safe for concurrent reads.
type YenPath[W Weight] struct {
	Nodes []graph.NodeID
	Cost  W
}

// YenKShortest computes up to k loopless shortest paths from src to
// dst on c using Yen's algorithm (1971). Returns paths sorted by
// total cost ascending; an empty slice when src cannot reach dst.
//
// The implementation runs at most k * (V + E) Dijkstra calls and is
// suitable for small-to-medium k (k <= a few hundred). For larger k
// or implicit-path representations, prefer Eppstein's algorithm
// (scheduled for a later task).
func YenKShortest[W Weight](c *csr.CSR[W], src, dst graph.NodeID, k int) []YenPath[W] {
	out, _ := YenKShortestCtx(context.Background(), c, src, dst, k)
	return out
}

// YenKShortestCtx is the context-aware variant of [YenKShortest].
// ctx.Err() is checked at every spur iteration; on cancellation
// returns (nil, wrapped ctx.Err()).
//
// Memory: the implementation allocates one O(V) scratch set
// (dist/parent/found/visited/excluded) and one O(E) edge-index map
// at entry, then reuses them across all internal Dijkstra calls.
// The v1.0 implementation reallocated all of these per spur step.
//
//nolint:gocyclo // canonical Yen: initial Dijkstra + k-1 spur rounds + candidate sort
func YenKShortestCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	if k <= 0 {
		return nil, nil
	}

	maxID := uint64(c.MaxNodeID())
	scr := newYenScratch[W](maxID)

	if err := DijkstraInto(ctx, c, src, scr.dist, scr.parent, scr.found); err != nil {
		return nil, err
	}
	if !scr.found[uint64(dst)] {
		return nil, nil
	}
	first := reconstructYenPath(scr.parent, src, dst)
	firstCost := scr.dist[uint64(dst)]
	result := []YenPath[W]{{Nodes: first, Cost: firstCost}}
	if k == 1 {
		return result, nil
	}

	edgeIdx := buildEdgeIndex[W](c)

	type candRef struct {
		start, end int
		cost       W
	}
	// candArena holds every candidate path's nodes concatenated; each
	// candidate is identified by a (start, end) range into the arena.
	// The arena grows monotonically across the k-1 rounds, so previous
	// indices stay valid until the function returns.
	candArena := make([]graph.NodeID, 0, 128)
	candidates := make([]candRef, 0, 16)
	banned := make(map[edgeKey]struct{}, 16)

	for i := 1; i < k; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prevPath := result[i-1].Nodes
		for spurIdx := 0; spurIdx < len(prevPath)-1; spurIdx++ {
			spurNode := prevPath[spurIdx]
			rootPath := prevPath[:spurIdx+1]
			clear(banned)
			fillBannedEdges(banned, result, rootPath, spurIdx)
			cand, ok := dijkstraAvoidingInto(ctx, c, spurNode, dst, banned, rootPath[:len(rootPath)-1], scr)
			if !ok {
				continue
			}
			rootCost := pathCostFast[W](c.WeightsSlice(), edgeIdx, rootPath)
			candStart := len(candArena)
			candArena = append(candArena, rootPath[:len(rootPath)-1]...)
			candArena = append(candArena, cand.Nodes...)
			candidates = append(candidates, candRef{
				start: candStart,
				end:   len(candArena),
				cost:  rootCost + cand.Cost,
			})
		}
		if len(candidates) == 0 {
			break
		}
		sort.Slice(candidates, func(a, b int) bool { return candidates[a].cost < candidates[b].cost })
		// Promote the cheapest candidate to result by materialising
		// an owned NodeID slice (the arena may grow further on the
		// next round and invalidate the reference's backing pointer).
		best := candidates[0]
		nodes := make([]graph.NodeID, best.end-best.start)
		copy(nodes, candArena[best.start:best.end])
		result = append(result, YenPath[W]{Nodes: nodes, Cost: best.cost})
		candidates = candidates[1:]
	}
	return result, nil
}

// edgeKey identifies a directed edge by its endpoints.
type edgeKey struct{ from, to graph.NodeID }

// yenScratch holds the per-Yen-call ephemeral working storage. All
// slices have length c.MaxNodeID(); the heap and pathBuf are reused
// across every internal Dijkstra call.
type yenScratch[W Weight] struct {
	dist     []W
	parent   []graph.NodeID
	found    []bool
	visited  []bool
	excluded []bool
	heap     dijkHeap[W]
	pathBuf  []graph.NodeID
}

func newYenScratch[W Weight](maxID uint64) *yenScratch[W] {
	return &yenScratch[W]{
		dist:     make([]W, maxID),
		parent:   make([]graph.NodeID, maxID),
		found:    make([]bool, maxID),
		visited:  make([]bool, maxID),
		excluded: make([]bool, maxID),
		pathBuf:  make([]graph.NodeID, 0, 32),
	}
}

// fillBannedEdges adds to banned the (u, v) edges that any previously
// returned path uses at the current spurIdx — these are forbidden
// for the next deviation. banned is expected to have been cleared by
// the caller; this function never allocates.
func fillBannedEdges[W Weight](banned map[edgeKey]struct{}, paths []YenPath[W], rootPath []graph.NodeID, spurIdx int) {
	for _, p := range paths {
		if len(p.Nodes) <= spurIdx+1 {
			continue
		}
		match := true
		for i := 0; i <= spurIdx; i++ {
			if i >= len(p.Nodes) || p.Nodes[i] != rootPath[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		banned[edgeKey{from: p.Nodes[spurIdx], to: p.Nodes[spurIdx+1]}] = struct{}{}
	}
}

// dijkstraAvoidingInto runs point-to-point Dijkstra from spur to dst
// while skipping banned edges and excluded intermediate nodes, using
// the caller-provided scratch. On success it returns a YenPath whose
// Nodes slice aliases scr.pathBuf (valid only until the next call);
// the caller must copy if the result needs to outlive the next spur
// iteration.
//
//nolint:gocyclo // canonical point-to-point Dijkstra with ban/exclude filters
func dijkstraAvoidingInto[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	spur, dst graph.NodeID,
	banned map[edgeKey]struct{},
	rootInterior []graph.NodeID,
	scr *yenScratch[W],
) (YenPath[W], bool) {
	var zeroPath YenPath[W]
	var zero W

	for i := range scr.dist {
		scr.dist[i] = zero
		scr.parent[i] = 0
		scr.found[i] = false
		scr.visited[i] = false
	}
	scr.heap.items = scr.heap.items[:0]
	for _, n := range rootInterior {
		scr.excluded[uint64(n)] = true
	}
	defer func() {
		for _, n := range rootInterior {
			scr.excluded[uint64(n)] = false
		}
	}()

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()

	scr.dist[uint64(spur)] = zero
	scr.found[uint64(spur)] = true
	scr.parent[uint64(spur)] = spur
	scr.heap.push(zero, spur)

	popCount := 0
	for scr.heap.len() > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return zeroPath, false
			}
		}
		popCount++
		top := scr.heap.pop()
		if top.node == dst {
			break
		}
		if scr.visited[uint64(top.node)] {
			continue
		}
		scr.visited[uint64(top.node)] = true
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			if _, isBanned := banned[edgeKey{from: top.node, to: nb}]; isBanned {
				continue
			}
			if scr.excluded[uint64(nb)] {
				continue
			}
			var w W
			if weights != nil {
				w = weights[k]
			}
			cand := top.dist + w
			if !scr.found[uint64(nb)] || cand < scr.dist[uint64(nb)] {
				scr.dist[uint64(nb)] = cand
				scr.parent[uint64(nb)] = top.node
				scr.found[uint64(nb)] = true
				scr.heap.push(cand, nb)
			}
		}
	}
	if !scr.found[uint64(dst)] {
		return zeroPath, false
	}
	length := 1
	for cur := dst; cur != spur; {
		cur = scr.parent[uint64(cur)]
		length++
	}
	if cap(scr.pathBuf) < length {
		scr.pathBuf = make([]graph.NodeID, length)
	} else {
		scr.pathBuf = scr.pathBuf[:length]
	}
	cur := dst
	for i := length - 1; i > 0; i-- {
		scr.pathBuf[i] = cur
		cur = scr.parent[uint64(cur)]
	}
	scr.pathBuf[0] = spur
	return YenPath[W]{Nodes: scr.pathBuf, Cost: scr.dist[uint64(dst)]}, true
}

// reconstructYenPath walks parents from dst back to src to materialise
// a freshly-allocated path. Used only for the first (initial) shortest
// path; subsequent spurs go through scr.pathBuf via copy in the caller.
func reconstructYenPath(parent []graph.NodeID, src, dst graph.NodeID) []graph.NodeID {
	length := 1
	for cur := dst; cur != src; {
		cur = parent[uint64(cur)]
		length++
	}
	out := make([]graph.NodeID, length)
	cur := dst
	for i := length - 1; i > 0; i-- {
		out[i] = cur
		cur = parent[uint64(cur)]
	}
	out[0] = src
	return out
}

// buildEdgeIndex constructs a (from, to) -> edge-index map covering
// every directed edge in c. For multigraphs the first occurrence of
// each (from, to) pair wins, matching the v1.0 pathCost semantics.
func buildEdgeIndex[W Weight](c *csr.CSR[W]) map[edgeKey]uint64 {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	maxID := uint64(c.MaxNodeID())
	idx := make(map[edgeKey]uint64, len(edges))
	for from := uint64(0); from < maxID; from++ {
		start := verts[from]
		end := verts[from+1]
		for k := start; k < end; k++ {
			key := edgeKey{from: graph.NodeID(from), to: edges[k]}
			if _, exists := idx[key]; !exists {
				idx[key] = k
			}
		}
	}
	return idx
}

// pathCostFast computes the total weight of path using a pre-built
// edge index. Cost is O(len(path)) versus O(len(path) * avgDeg) for
// the linear-scan equivalent.
func pathCostFast[W Weight](weights []W, edgeIdx map[edgeKey]uint64, path []graph.NodeID) W {
	var cost W
	for i := 0; i < len(path)-1; i++ {
		idx, ok := edgeIdx[edgeKey{from: path[i], to: path[i+1]}]
		if !ok {
			continue
		}
		if weights != nil {
			cost += weights[idx]
		}
	}
	return cost
}
