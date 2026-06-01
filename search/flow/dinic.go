// Package flow implements network-flow algorithms over directed
// capacitated graphs. v1 carries Dinic's max-flow + Stoer-Wagner
// global min-cut; future work will add min-cost max-flow.
package flow

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Network is a directed capacitated graph. It is stored as a sum of
// forward and reverse-edge adjacency lists (one slice per node) so
// residual updates run in O(1) per edge.
//
// Concurrency: Network is not safe for concurrent mutation; AddEdge
// and MaxFlow share the residual-capacity arrays without
// synchronisation. Use a separate Network per goroutine, or
// serialise externally.
//
// v1 limitation. Network's capacity type is int (not generic over
// the Weight constraint). Genericising over W requires a
// representable "infinity push" sentinel; Go's generics cannot
// produce that cleanly for arbitrary named numeric types. Callers
// needing other weight types should map their capacities to int.
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
	defer metrics.Time("search.flow.MaxFlow")()
	out, _ := MaxFlowCtx(context.Background(), g, src, sink)
	return out
}

// MaxFlowCtx is the context-aware variant of [MaxFlow]. ctx.Err() is
// checked at every BFS-level rebuild (the outer Dinic phase boundary)
// AND inside the inner DFS augment at a 1<<12-step granularity, so
// cancellation latency stays bounded even on a dense network whose
// phases dominate the wall time. On cancellation returns
// (totalSoFar, wrapped ctx.Err()).
func MaxFlowCtx(ctx context.Context, g *Network, src, sink int) (int, error) {
	defer metrics.Time("search.flow.MaxFlowCtx")()
	level := make([]int, g.N())
	iter := make([]int, g.N())
	stack := make([]int, 0, g.N())
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.flow.MaxFlowCtx.errors", 1)
			return total, err
		}
		if !buildLevel(g, src, sink, level) {
			return total, nil
		}
		for i := range iter {
			iter[i] = 0
		}
		for {
			var f int
			var aerr error
			f, stack, aerr = augmentFlow(ctx, g, src, sink, 1<<62, level, iter, stack)
			if aerr != nil {
				metrics.IncCounter("search.flow.MaxFlowCtx.errors", 1)
				return total, aerr
			}
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
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		for _, e := range g.heads[v] {
			if g.cap[e] > 0 && level[g.edgeTo[e]] < 0 {
				level[g.edgeTo[e]] = level[v] + 1
				queue = append(queue, g.edgeTo[e])
			}
		}
	}
	return level[sink] >= 0
}

// augmentFlow walks one augmenting path from src to sink in the
// current Dinic level graph using an explicit DFS stack, returning
// the flow pushed (0 if no path remains). The stack slice is reused
// across calls: callers should pass the slice from the previous call
// and capture the returned (possibly grown) slice header.
//
// Iterative by design — recursion would blow up on deep layered
// residual graphs at LDBC-SF10 scale (CLAUDE.md mandate).
//
// ctx cancellation is checked at a 1<<12-iteration granularity to
// keep latency bounded on dense phases while not adding measurable
// overhead to the inner loop (one and-mask + one ctx.Err() call per
// 4096 stack ops).
func augmentFlow(ctx context.Context, g *Network, src, sink, pushLim int, level, iter, stack []int) (pushed int, stackOut []int, err error) {
	stack = append(stack[:0], src)
	tick := 0
	for len(stack) > 0 {
		tick++
		if tick&0xFFF == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return 0, stack, cerr
			}
		}
		v := stack[len(stack)-1]
		if v == sink {
			// Compute the bottleneck capacity along the stack.
			push := pushLim
			for i := 0; i < len(stack)-1; i++ {
				u := stack[i]
				e := g.heads[u][iter[u]]
				if g.cap[e] < push {
					push = g.cap[e]
				}
			}
			// Apply the push to every edge on the path.
			for i := 0; i < len(stack)-1; i++ {
				u := stack[i]
				e := g.heads[u][iter[u]]
				g.cap[e] -= push
				g.cap[e^1] += push
			}
			return push, stack, nil
		}
		descended := false
		for ; iter[v] < len(g.heads[v]); iter[v]++ {
			e := g.heads[v][iter[v]]
			if g.cap[e] <= 0 || level[g.edgeTo[e]] != level[v]+1 {
				continue
			}
			stack = append(stack, g.edgeTo[e])
			descended = true
			break
		}
		if !descended {
			// Dead-end at v: pop and advance the parent's iter past
			// the edge that led here.
			stack = stack[:len(stack)-1]
			if len(stack) > 0 {
				iter[stack[len(stack)-1]]++
			}
		}
	}
	return 0, stack, nil
}
