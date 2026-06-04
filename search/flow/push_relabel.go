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
	defer metrics.Time("search.flow.PushRelabelMaxFlow")()
	out, _ := PushRelabelMaxFlowCtx(context.Background(), g, src, sink)
	return out
}

// PushRelabelMaxFlowCtx is the context-aware variant of
// [PushRelabelMaxFlow]. ctx.Err() is checked every 4096 discharges;
// on cancellation returns (totalSoFar, wrapped ctx.Err()). totalSoFar
// is the excess accumulated at sink — a valid lower bound on the
// true max-flow.
//
// Before any work it validates that the capacities cannot overflow the
// int64 excess accumulation, returning (0, [ErrCapacityOverflow]) when
// they could.
//
//nolint:gocyclo // textbook FIFO push-relabel with gap heuristic
func PushRelabelMaxFlowCtx(ctx context.Context, g *Network, src, sink int) (int, error) {
	defer metrics.Time("search.flow.PushRelabelMaxFlowCtx")()
	n := g.N()
	if n == 0 || src == sink {
		return 0, nil
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

	tick := 0
	for qh := 0; qh < len(queue); qh++ {
		tick++
		if tick&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.flow.PushRelabelMaxFlowCtx.errors", 1)
				return excess[sink], err
			}
		}
		v := queue[qh]
		inQueue[v] = false
		discharge(g, v, src, sink, height, excess, countH, current, &queue, inQueue, n)
	}
	return excess[sink], nil
}

func discharge(g *Network, v, src, sink int, height, excess, countH, current []int, queue *[]int, inQueue []bool, n int) {
	for excess[v] > 0 {
		if current[v] >= len(g.heads[v]) {
			relabel(g, v, height, countH, n)
			current[v] = 0
			if height[v] >= n {
				return
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
