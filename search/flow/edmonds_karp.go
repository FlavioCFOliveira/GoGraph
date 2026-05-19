package flow

import "context"

// EdmondsKarp computes the max-flow from src to sink in g using the
// Edmonds-Karp algorithm (Ford-Fulkerson with BFS-discovered
// augmenting paths). Complexity is O(V * E^2), which is worse than
// [MaxFlow]'s Dinic-based bound for general networks but simpler in
// structure — useful as a reference implementation and a baseline
// for property testing.
func EdmondsKarp(g *Network, src, sink int) int {
	out, _ := EdmondsKarpCtx(context.Background(), g, src, sink)
	return out
}

// EdmondsKarpCtx is the context-aware variant of [EdmondsKarp].
// ctx.Err() is checked at every BFS rebuild (the outer augmenting-
// path boundary); on cancellation returns (totalSoFar, wrapped
// ctx.Err()).
func EdmondsKarpCtx(ctx context.Context, g *Network, src, sink int) (int, error) {
	n := g.N()
	parentEdge := make([]int, n)
	queue := make([]int, 0, n)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// BFS for an augmenting path. parentEdge[v] holds the index
		// of the edge that reached v from its BFS predecessor; the
		// special value -1 marks "unvisited" (we additionally treat
		// parentEdge[src] = -1 to detect the source on path walk).
		for i := range parentEdge {
			parentEdge[i] = -1
		}
		queue = append(queue[:0], src)
		for qh := 0; qh < len(queue) && parentEdge[sink] == -1; qh++ {
			v := queue[qh]
			for _, e := range g.heads[v] {
				w := g.edgeTo[e]
				if g.cap[e] <= 0 || w == src || parentEdge[w] != -1 {
					continue
				}
				parentEdge[w] = e
				if w == sink {
					break
				}
				queue = append(queue, w)
			}
		}
		if parentEdge[sink] == -1 {
			return total, nil
		}
		// Walk back from sink to src to find the bottleneck.
		push := 1 << 62
		for v := sink; v != src; {
			e := parentEdge[v]
			if g.cap[e] < push {
				push = g.cap[e]
			}
			v = g.edgeTo[e^1]
		}
		// Apply the push to every edge on the augmenting path.
		for v := sink; v != src; {
			e := parentEdge[v]
			g.cap[e] -= push
			g.cap[e^1] += push
			v = g.edgeTo[e^1]
		}
		total += push
	}
}
