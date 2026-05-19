package search

import (
	"context"
	"sort"

	"gograph/graph"
	"gograph/graph/csr"
)

// YenPath is one shortest path produced by [YenKShortest].
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
//nolint:gocyclo // canonical Yen: initial Dijkstra + k-1 spur rounds + candidate sort
func YenKShortestCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	if k <= 0 {
		return nil, nil
	}
	d, err := DijkstraCtx(ctx, c, src)
	if err != nil {
		return nil, err
	}
	first := d.Path(dst)
	if first == nil {
		return nil, nil
	}
	firstCost, _ := d.Distance(dst)
	result := []YenPath[W]{{Nodes: first, Cost: firstCost}}
	if k == 1 {
		return result, nil
	}

	candidates := []YenPath[W]{}
	for i := 1; i < k; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prevPath := result[i-1].Nodes
		for spurIdx := 0; spurIdx < len(prevPath)-1; spurIdx++ {
			spurNode := prevPath[spurIdx]
			rootPath := append([]graph.NodeID(nil), prevPath[:spurIdx+1]...)
			banned := bannedEdges(result, rootPath, spurIdx)
			cand := dijkstraAvoiding(c, spurNode, dst, banned, rootPath[:len(rootPath)-1])
			if cand == nil {
				continue
			}
			rootCost := pathCost[W](c, rootPath)
			candidates = append(candidates, YenPath[W]{
				Nodes: append(rootPath[:len(rootPath)-1], cand.Nodes...),
				Cost:  rootCost + cand.Cost,
			})
		}
		if len(candidates) == 0 {
			break
		}
		sort.Slice(candidates, func(a, b int) bool { return candidates[a].Cost < candidates[b].Cost })
		result = append(result, candidates[0])
		candidates = candidates[1:]
	}
	return result, nil
}

// edgeKey identifies a directed edge by its endpoints.
type edgeKey struct{ from, to graph.NodeID }

// bannedEdges returns the set of (u, v) edges that any previously
// returned path uses at the current spurIdx — these are forbidden
// for the next deviation.
func bannedEdges[W Weight](paths []YenPath[W], rootPath []graph.NodeID, spurIdx int) map[edgeKey]struct{} {
	out := make(map[edgeKey]struct{})
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
		out[edgeKey{from: p.Nodes[spurIdx], to: p.Nodes[spurIdx+1]}] = struct{}{}
	}
	return out
}

// dijkstraAvoiding runs Dijkstra from spur to dst while skipping the
// edges in banned and the intermediate nodes in rootInterior.
//
// Reachability is tracked via an explicit found[] bitmap rather than
// an in-band +Inf sentinel — this avoids overflow/wraparound on
// integer weight types (the v1.0.0 implementation built the sentinel
// by 60 iterations of "v += v" which wraps mod 2^64 on uint64 and
// saturates on float32).
func dijkstraAvoiding[W Weight](c *csr.CSR[W], spur, dst graph.NodeID, banned map[edgeKey]struct{}, rootInterior []graph.NodeID) *YenPath[W] {
	excluded := make(map[graph.NodeID]struct{}, len(rootInterior))
	for _, n := range rootInterior {
		excluded[n] = struct{}{}
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	maxID := uint64(c.MaxNodeID())
	dist := make([]W, maxID)
	parent := make([]graph.NodeID, maxID)
	visited := make([]bool, maxID)
	found := make([]bool, maxID)
	dist[uint64(spur)] = 0
	found[uint64(spur)] = true
	h := &dijkHeap[W]{}
	h.push(0, spur)
	for h.len() > 0 {
		top := h.pop()
		if top.node == dst {
			break
		}
		if visited[uint64(top.node)] {
			continue
		}
		visited[uint64(top.node)] = true
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			if _, banned := banned[edgeKey{from: top.node, to: nb}]; banned {
				continue
			}
			if _, ex := excluded[nb]; ex {
				continue
			}
			var w W
			if weights != nil {
				w = weights[k]
			}
			cand := top.dist + w
			if !found[uint64(nb)] || cand < dist[uint64(nb)] {
				dist[uint64(nb)] = cand
				parent[uint64(nb)] = top.node
				found[uint64(nb)] = true
				h.push(cand, nb)
			}
		}
	}
	if !found[uint64(dst)] {
		return nil
	}
	length := 1
	for cur := dst; cur != spur; {
		cur = parent[uint64(cur)]
		length++
	}
	out := make([]graph.NodeID, length)
	cur := dst
	for i := length - 1; i > 0; i-- {
		out[i] = cur
		cur = parent[uint64(cur)]
	}
	out[0] = spur
	return &YenPath[W]{Nodes: out, Cost: dist[uint64(dst)]}
}

func pathCost[W Weight](c *csr.CSR[W], path []graph.NodeID) W {
	var cost W
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	for i := 0; i < len(path)-1; i++ {
		from := uint64(path[i])
		to := path[i+1]
		start := verts[from]
		end := verts[from+1]
		for k := start; k < end; k++ {
			if edges[k] == to {
				if weights != nil {
					cost += weights[k]
				}
				break
			}
		}
	}
	return cost
}
