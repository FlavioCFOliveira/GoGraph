package flow

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// PushRelabelMaxFlow computes the max-flow from src to sink in g
// using the FIFO push-relabel algorithm (Goldberg-Tarjan 1988) with
// the gap heuristic. Empirically the fastest practical max-flow on
// dense networks (worst-case O(V^2 * sqrt(E)) with the gap pruning).
//
// The network's edges are mutated in place (the residual capacities
// are decremented and the reverse edges incremented); callers
// needing to re-run the algorithm should rebuild the network.
//
// If the network's capacities could overflow the int64 excess
// accumulation (see [ErrCapacityOverflow]), PushRelabelMaxFlow returns
// 0 rather than a wrapped value; use [PushRelabelMaxFlowCtx] to receive
// the typed error.
func PushRelabelMaxFlow(g *Network, src, sink int) int {
	defer metrics.Time("search.flow.PushRelabelMaxFlow").Stop()
	out, _ := PushRelabelMaxFlowCtx(context.Background(), g, src, sink)
	return out
}

// PushRelabelMaxFlowCtx is the context-aware variant of
// [PushRelabelMaxFlow]. ctx.Err() is checked before the first unit of
// work and every 4096 units thereafter, where a unit is one queue pop
// or one step of a discharge; on cancellation returns (totalSoFar, the
// raw ctx.Err()). totalSoFar is the excess accumulated at sink — a
// valid lower bound on the true max-flow, and still one when the
// cancellation is observed part-way through a discharge, because sink
// is never enqueued and so flow never leaves it.
//
// Before any work it validates that the capacities cannot overflow the
// int64 excess accumulation, returning (0, [ErrCapacityOverflow]) when
// they could.
//
// textbook FIFO push-relabel with gap heuristic
func PushRelabelMaxFlowCtx(ctx context.Context, g *Network, src, sink int) (int, error) {
	defer metrics.Time("search.flow.PushRelabelMaxFlowCtx").Stop()
	n := g.N()
	if err := validateEndpoints(n, src, sink); err != nil {
		metrics.IncCounter("search.flow.PushRelabelMaxFlowCtx.errors", 1)
		return 0, err
	}
	if err := validateCapacities(g, src); err != nil {
		metrics.IncCounter("search.flow.PushRelabelMaxFlowCtx.errors", 1)
		return 0, err
	}
	height := make([]int, n)
	excess := make([]int, n)
	// countH[h] = number of vertices at height h. Required by the
	// gap heuristic: if countH[h] drops to 0 for some h < n, every
	// vertex above h is permanently disconnected from sink and can
	// be raised to n.
	countH := make([]int, 2*n+1)
	current := make([]int, n) // per-vertex pointer into g.heads
	inQueue := make([]bool, n)
	queue := make([]int, 0, n)

	height[src] = n
	countH[n]++
	for i := 0; i < n; i++ {
		if i != src {
			countH[0]++
		}
	}
	// Initial preflow: saturate every edge out of src.
	for _, e := range g.heads[src] {
		c := g.cap[e]
		if c <= 0 {
			continue
		}
		dst := g.edgeTo[e]
		g.cap[e] = 0
		g.cap[e^1] += c
		excess[dst] += c
		if dst != sink && !inQueue[dst] {
			queue = append(queue, dst)
			inQueue[dst] = true
		}
	}

	// tick counts UNITS OF WORK, not queue pops, and is threaded through
	// discharge so that the work between two consecutive polls is bounded by
	// the stride rather than by 4096 * (cost of one discharge). A single
	// discharge is O(V * deg(v)) and was measured at 600,004 inner steps on a
	// 200,003-vertex fan-out network -- 146x the whole stride -- with no poll
	// inside it, so counting discharges alone left the interval between polls
	// unbounded in the input size (rmp #2593).
	tick := 0
	for qh := 0; qh < len(queue); qh++ {
		// Check the stride mask BEFORE incrementing, so the poll lands on
		// iteration 0; the reverse order left every network that finished in
		// under 4096 discharges returning the TRUE, COMPLETE max flow with a
		// nil error under a dead context. The check-then-increment idiom
		// matches search/dijkstra.go and search/prim.go.
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.flow.PushRelabelMaxFlowCtx.errors", 1)
				return excess[sink], err
			}
		}
		tick++
		v := queue[qh]
		inQueue[v] = false
		var err error
		tick, err = discharge(ctx, g, v, src, sink, height, excess, countH, current, &queue, inQueue, n, tick)
		if err != nil {
			metrics.IncCounter("search.flow.PushRelabelMaxFlowCtx.errors", 1)
			return excess[sink], err
		}
	}
	return excess[sink], nil
}

// discharge drains the excess at v, pushing along admissible edges and
// relabelling when none remain. It carries the caller's work counter in and
// out (by value, so no allocation and no pointer traffic in the loop) and
// polls ctx on the same 4096-unit stride as the FIFO queue loop: one
// discharge is O(V * deg(v)) and therefore cannot be treated as a single
// unit of work without leaving the cancellation interval unbounded in the
// input size.
//
// On cancellation it stops where it stands and returns the context error. The
// residual network is left part-way through the discharge, which the entry
// point's contract already permits (the edges are mutated in place and the
// caller must rebuild the network to re-run). excess[sink] remains a valid
// lower bound on the true max-flow, because sink is never enqueued -- neither
// the initial preflow nor the push follow-up enqueues it -- so discharge is
// never called for sink and no push ever moves flow back out of it.
func discharge(ctx context.Context, g *Network, v, src, sink int, height, excess, countH, current []int, queue *[]int, inQueue []bool, n, tick int) (int, error) {
	for excess[v] > 0 {
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return tick, err
			}
		}
		tick++
		if current[v] >= len(g.heads[v]) {
			relabel(g, v, height, countH, n)
			current[v] = 0
			if height[v] >= n {
				return tick, nil
			}
			continue
		}
		e := g.heads[v][current[v]]
		w := g.edgeTo[e]
		if g.cap[e] > 0 && height[v] == height[w]+1 {
			push(g, v, w, e, excess)
			if w != src && w != sink && excess[w] > 0 && !inQueue[w] {
				*queue = append(*queue, w)
				inQueue[w] = true
			}
		} else {
			current[v]++
		}
	}
	return tick, nil
}

func push(g *Network, v, w, e int, excess []int) {
	send := excess[v]
	if g.cap[e] < send {
		send = g.cap[e]
	}
	g.cap[e] -= send
	g.cap[e^1] += send
	excess[v] -= send
	excess[w] += send
	_ = v
	_ = w
}

func relabel(g *Network, v int, height, countH []int, n int) {
	oldHeight := height[v]
	newH := 2*n + 1
	for _, e := range g.heads[v] {
		if g.cap[e] > 0 {
			h := height[g.edgeTo[e]] + 1
			if h < newH {
				newH = h
			}
		}
	}
	if newH > 2*n {
		newH = n
	}
	countH[oldHeight]--
	if oldHeight > 0 && oldHeight < n && countH[oldHeight] == 0 {
		// Gap heuristic: every vertex with height in
		// (oldHeight, n) can no longer reach sink — raise them.
		for u := 0; u < n; u++ {
			if height[u] > oldHeight && height[u] < n {
				countH[height[u]]--
				height[u] = n
				countH[n]++
			}
		}
	}
	height[v] = newH
	countH[newH]++
}
