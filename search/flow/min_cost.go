package flow

import (
	"container/heap"
	"context"

	"gograph/internal/metrics"
)

// CostNetwork extends [Network] with a per-edge cost. AddEdge takes
// a (capacity, cost) pair; reverse edges receive cost = -cost so the
// residual cost of cancelling forward flow is correctly reflected.
//
// Concurrency contract identical to [Network]: not safe for
// concurrent mutation; one CostNetwork per goroutine.
type CostNetwork struct {
	*Network
	cost []int
}

// NewCostNetwork returns an empty cost-network with n nodes.
func NewCostNetwork(n int) *CostNetwork {
	return &CostNetwork{Network: NewNetwork(n)}
}

// AddCostEdge inserts a directed edge from src to dst with the given
// capacity and per-unit cost. The reverse edge is created with zero
// capacity and -cost.
func (g *CostNetwork) AddCostEdge(src, dst, capacity, cost int) {
	g.heads[src] = append(g.heads[src], len(g.edgeTo))
	g.edgeTo = append(g.edgeTo, dst)
	g.cap = append(g.cap, capacity)
	g.cost = append(g.cost, cost)
	g.heads[dst] = append(g.heads[dst], len(g.edgeTo))
	g.edgeTo = append(g.edgeTo, src)
	g.cap = append(g.cap, 0)
	g.cost = append(g.cost, -cost)
}

// MinCostMaxFlow runs Successive Shortest Paths on g, pushing flow
// along the cheapest augmenting path discovered by Dijkstra over
// reduced edge costs. Returns (totalFlow, totalCost). Cost may be
// negative when the network contains arbitrage cycles, but the
// algorithm assumes a feasible solution exists (no negative cycle).
//
// SSP runs at most O(V * E) Dijkstras for unit-capacity networks
// and O(F * E log V) for general networks where F is the resulting
// flow magnitude — adequate for assignment-style problems with
// modest source/sink counts.
func MinCostMaxFlow(g *CostNetwork, src, sink int) (flow, cost int) {
	defer metrics.Time("search.flow.MinCostMaxFlow")()
	out, c, _ := MinCostMaxFlowCtx(context.Background(), g, src, sink)
	return out, c
}

// MinCostMaxFlowCtx is the context-aware variant of [MinCostMaxFlow].
// ctx.Err() is checked at every SSP iteration; on cancellation
// returns (partialFlow, partialCost, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical SSP with potentials: init + per-iteration Dijkstra + augmentation
func MinCostMaxFlowCtx(ctx context.Context, g *CostNetwork, src, sink int) (totalFlow, totalCost int, err error) {
	defer metrics.Time("search.flow.MinCostMaxFlowCtx")()
	n := g.N()
	if n == 0 || src == sink {
		return 0, 0, nil
	}
	// Potentials initialise to 0. With non-negative costs this is
	// valid; with negative-but-no-negative-cycle inputs a Bellman-
	// Ford warmup would be required.
	potential := make([]int, n)
	dist := make([]int, n)
	parentEdge := make([]int, n)
	for {
		if cerr := ctx.Err(); cerr != nil {
			metrics.IncCounter("search.flow.MinCostMaxFlowCtx.errors", 1)
			return totalFlow, totalCost, cerr
		}
		// Dijkstra on reduced costs.
		const inf = 1 << 62
		for i := range dist {
			dist[i] = inf
			parentEdge[i] = -1
		}
		dist[src] = 0
		pq := &mcmfPQ{}
		heap.Push(pq, mcmfItem{node: src, dist: 0})
		for pq.Len() > 0 {
			it := heap.Pop(pq).(mcmfItem) //nolint:errcheck // statically known
			if it.dist > dist[it.node] {
				continue
			}
			for _, e := range g.heads[it.node] {
				if g.cap[e] <= 0 {
					continue
				}
				to := g.edgeTo[e]
				rc := g.cost[e] + potential[it.node] - potential[to]
				if rc < 0 {
					rc = 0
				}
				cand := it.dist + rc
				if cand < dist[to] {
					dist[to] = cand
					parentEdge[to] = e
					heap.Push(pq, mcmfItem{node: to, dist: cand})
				}
			}
		}
		if dist[sink] == inf {
			return totalFlow, totalCost, nil
		}
		// Update potentials.
		for i := range potential {
			if dist[i] < inf {
				potential[i] += dist[i]
			}
		}
		// Bottleneck along the path.
		push := inf
		for v := sink; v != src; {
			e := parentEdge[v]
			if g.cap[e] < push {
				push = g.cap[e]
			}
			v = g.edgeTo[e^1]
		}
		// Apply.
		for v := sink; v != src; {
			e := parentEdge[v]
			g.cap[e] -= push
			g.cap[e^1] += push
			totalCost += push * g.cost[e]
			v = g.edgeTo[e^1]
		}
		totalFlow += push
	}
}

type mcmfItem struct {
	dist int
	node int
}

type mcmfPQ []mcmfItem

func (h mcmfPQ) Len() int           { return len(h) }
func (h mcmfPQ) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h mcmfPQ) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *mcmfPQ) Push(x any)        { *h = append(*h, x.(mcmfItem)) } //nolint:errcheck // statically known
func (h *mcmfPQ) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
