// Package flow implements network-flow algorithms over directed
// capacitated graphs. v1 carries Dinic's max-flow + Stoer-Wagner
// global min-cut; future work will add min-cost max-flow.
package flow

import "context"

// Network is a directed capacitated graph. It is stored as a sum of
// forward and reverse-edge adjacency lists (one slice per node) so
// residual updates run in O(1) per edge.
type Network struct {
	heads  [][]int // edge indices per node (forward+reverse adjacency)
	edgeTo []int   // destination of each edge
	cap    []int   // remaining residual capacity per edge
}

// NewNetwork returns an empty network with n nodes.
func NewNetwork(n int) *Network {
	return &Network{heads: make([][]int, n)}
}

// N returns the node count.
func (g *Network) N() int { return len(g.heads) }

// AddEdge inserts a directed edge from src to dst with the given
// capacity. Internally a reverse edge with zero capacity is added
// so residuals are O(1) lookups.
func (g *Network) AddEdge(src, dst, capacity int) {
	g.heads[src] = append(g.heads[src], len(g.edgeTo))
	g.edgeTo = append(g.edgeTo, dst)
	g.cap = append(g.cap, capacity)
	g.heads[dst] = append(g.heads[dst], len(g.edgeTo))
	g.edgeTo = append(g.edgeTo, src)
	g.cap = append(g.cap, 0)
}

// MaxFlow runs Dinic's algorithm from src to sink and returns the
// total flow. Complexity O(V^2 * E) general; O(E * sqrt(V)) on
// unit-capacity networks.
func MaxFlow(g *Network, src, sink int) int {
	out, _ := MaxFlowCtx(context.Background(), g, src, sink)
	return out
}

// MaxFlowCtx is the context-aware variant of [MaxFlow]. ctx.Err() is
// checked at every BFS-level rebuild (the outer Dinic phase boundary);
// on cancellation returns (totalSoFar, wrapped ctx.Err()).
func MaxFlowCtx(ctx context.Context, g *Network, src, sink int) (int, error) {
	level := make([]int, g.N())
	iter := make([]int, g.N())
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if !buildLevel(g, src, sink, level) {
			return total, nil
		}
		for i := range iter {
			iter[i] = 0
		}
		for {
			f := augmentFlow(g, src, sink, 1<<62, level, iter)
			if f == 0 {
				break
			}
			total += f
		}
	}
}

func buildLevel(g *Network, src, sink int, level []int) bool {
	for i := range level {
		level[i] = -1
	}
	level[src] = 0
	queue := []int{src}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, e := range g.heads[v] {
			if g.cap[e] > 0 && level[g.edgeTo[e]] < 0 {
				level[g.edgeTo[e]] = level[v] + 1
				queue = append(queue, g.edgeTo[e])
			}
		}
	}
	return level[sink] >= 0
}

func augmentFlow(g *Network, v, sink, push int, level, iter []int) int {
	if v == sink {
		return push
	}
	for ; iter[v] < len(g.heads[v]); iter[v]++ {
		e := g.heads[v][iter[v]]
		if g.cap[e] <= 0 || level[g.edgeTo[e]] != level[v]+1 {
			continue
		}
		got := augmentFlow(g, g.edgeTo[e], sink, minInt(push, g.cap[e]), level, iter)
		if got > 0 {
			g.cap[e] -= got
			g.cap[e^1] += got
			return got
		}
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
