package search

import (
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
	if k <= 0 {
		return nil
	}
	d, err := Dijkstra(c, src)
	if err != nil {
		return nil
	}
	first := d.Path(dst)
	if first == nil {
		return nil
	}
	firstCost, _ := d.Distance(dst)
	result := []YenPath[W]{{Nodes: first, Cost: firstCost}}
	if k == 1 {
		return result
	}

	candidates := []YenPath[W]{}
	for i := 1; i < k; i++ {
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
	return result
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
func dijkstraAvoiding[W Weight](c *csr.CSR[W], spur, dst graph.NodeID, banned map[edgeKey]struct{}, rootInterior []graph.NodeID) *YenPath[W] {
	excluded := make(map[graph.NodeID]struct{}, len(rootInterior))
	for _, n := range rootInterior {
		excluded[n] = struct{}{}
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	maxID := uint64(c.MaxNodeID())
	var inf W
	{
		var zero W
		inf = zero
		// Use a very-large sentinel by repeated addition.
		for i := 0; i < 60; i++ {
			inf++
			inf += inf
		}
	}
	dist := make([]W, maxID)
	parent := make([]graph.NodeID, maxID)
	visited := make([]bool, maxID)
	for i := range dist {
		dist[i] = inf
	}
	dist[uint64(spur)] = 0
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
			if cand < dist[uint64(nb)] {
				dist[uint64(nb)] = cand
				parent[uint64(nb)] = top.node
				h.push(cand, nb)
			}
		}
	}
	if dist[uint64(dst)] == inf {
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
