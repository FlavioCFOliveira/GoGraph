package flow

import (
	"container/heap"
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrNegativeCycle is returned by [MinCostMaxFlowCtx] when the
// Bellman-Ford bootstrap on a network containing negative-cost arcs
// detects a negative-cost cycle reachable from the source. SSP cannot
// converge in that case and the caller must restructure the network.
var ErrNegativeCycle = errors.New("flow: MinCostMaxFlow detected a negative cycle in the residual graph")

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
// reduced edge costs. Returns (totalFlow, totalCost).
//
// Negative arc costs are supported: when any arc has cost<0 the
// algorithm runs a Bellman-Ford bootstrap on the residual graph to
// initialise the node potentials so that reduced costs are
// non-negative from the first Dijkstra round onward. A negative-cost
// cycle reachable from src is reported by [MinCostMaxFlowCtx] via
// [ErrNegativeCycle]; this non-context entry point silently returns
// (0, 0) in that case.
//
// SSP runs at most O(V * E) Dijkstras for unit-capacity networks
// and O(F * E log V) for general networks where F is the resulting
// flow magnitude — adequate for assignment-style problems with
// modest source/sink counts.
//
// If the network's capacities or capacity-times-cost products could
// overflow the int64 flow/cost accumulation (see
// [ErrCapacityOverflow]), MinCostMaxFlow returns (0, 0) rather than
// wrapped values; use [MinCostMaxFlowCtx] to receive the typed error.
func MinCostMaxFlow(g *CostNetwork, src, sink int) (flow, cost int) {
	defer metrics.Time("search.flow.MinCostMaxFlow")()
	out, c, _ := MinCostMaxFlowCtx(context.Background(), g, src, sink)
	return out, c
}

// MinCostMaxFlowCtx is the context-aware variant of [MinCostMaxFlow].
// ctx.Err() is checked at every SSP iteration; on cancellation
// returns (partialFlow, partialCost, wrapped ctx.Err()).
//
// Negative arc costs are supported via a Bellman-Ford bootstrap; if a
// negative cycle is reachable from src, returns
// (0, 0, [ErrNegativeCycle]) without performing any augmentation.
//
// Before any work it validates that the capacities cannot overflow the
// int64 flow accumulation and that the worst-case total cost
// (source-cut capacity times the largest absolute per-unit cost) fits
// int64, returning (0, 0, [ErrCapacityOverflow]) otherwise.
//
//nolint:gocyclo // canonical SSP with potentials: BF bootstrap + per-iteration Dijkstra + augmentation
func MinCostMaxFlowCtx(ctx context.Context, g *CostNetwork, src, sink int) (totalFlow, totalCost int, err error) {
	defer metrics.Time("search.flow.MinCostMaxFlowCtx")()
	n := g.N()
	if n == 0 || src == sink {
		return 0, 0, nil
	}
	if verr := validateCostCapacities(g, src); verr != nil {
		metrics.IncCounter("search.flow.MinCostMaxFlowCtx.errors", 1)
		return 0, 0, verr
	}
	potential := make([]int, n)
	// Bootstrap potentials if any arc has negative cost. With purely
	// non-negative costs the zero-initialised potential vector
	// already satisfies the SSP invariant rc>=0.
	if hasNegativeCost(g) {
		pot, berr := bellmanFordBootstrap(g, src)
		if berr != nil {
			metrics.IncCounter("search.flow.MinCostMaxFlowCtx.errors", 1)
			return 0, 0, berr
		}
		potential = pot
	}
	dist := make([]int, n)
	parentEdge := make([]int, n)
	for {
		if cerr := ctx.Err(); cerr != nil {
			metrics.IncCounter("search.flow.MinCostMaxFlowCtx.errors", 1)
			return totalFlow, totalCost, cerr
		}
		// Dijkstra on reduced costs.
		const inf = capInf
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
				// SSP invariant: after a correct potential update the
				// reduced cost on every residual arc with cap>0 is
				// non-negative. A negative rc here is a programmer
				// error — bootstrap should have caught the negative-
				// cycle case, and the potential update at the bottom
				// of every iteration preserves the invariant.
				if rc < 0 {
					metrics.IncCounter("search.flow.MinCostMaxFlowCtx.errors", 1)
					return totalFlow, totalCost, fmt.Errorf(
						"flow: MinCostMaxFlow internal invariant violated: rc=%d (cost=%d, potU=%d, potV=%d, u=%d, v=%d)",
						rc, g.cost[e], potential[it.node], potential[to], it.node, to,
					)
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
		// Update potentials: only for nodes the Dijkstra reached.
		// Leaving unreachable nodes' potentials untouched preserves
		// the property that their (unused) arcs are never
		// rc-evaluated, since those nodes are never popped.
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

// hasNegativeCost reports whether any forward (cap>0) residual arc
// carries a strictly negative cost. Reverse arcs always start with
// cap=0, so they are skipped on the initial pass.
func hasNegativeCost(g *CostNetwork) bool {
	for e := range g.cost {
		if g.cap[e] > 0 && g.cost[e] < 0 {
			return true
		}
	}
	return false
}

// bellmanFordBootstrap computes shortest-path distances on the
// residual graph of g, restricted to arcs with cap>0, starting from
// src. Returns the potential vector to install in
// [MinCostMaxFlowCtx]; unreachable nodes carry the zero potential.
// Returns [ErrNegativeCycle] if a negative-cost cycle reachable from
// src is detected.
//
//nolint:gocyclo // canonical Bellman-Ford: V-1 relaxation passes + one cycle-detection pass + cleanup
func bellmanFordBootstrap(g *CostNetwork, src int) ([]int, error) {
	n := g.N()
	const inf = capInf
	dist := make([]int, n)
	for i := range dist {
		dist[i] = inf
	}
	dist[src] = 0
	for iter := 0; iter < n-1; iter++ {
		changed := false
		for u := 0; u < n; u++ {
			if dist[u] >= inf {
				continue
			}
			for _, e := range g.heads[u] {
				if g.cap[e] <= 0 {
					continue
				}
				v := g.edgeTo[e]
				cand := dist[u] + g.cost[e]
				if cand < dist[v] {
					dist[v] = cand
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	// One additional relaxation pass; any further improvement reveals
	// a negative cycle reachable from src.
	for u := 0; u < n; u++ {
		if dist[u] >= inf {
			continue
		}
		for _, e := range g.heads[u] {
			if g.cap[e] <= 0 {
				continue
			}
			v := g.edgeTo[e]
			if dist[u]+g.cost[e] < dist[v] {
				return nil, ErrNegativeCycle
			}
		}
	}
	// Replace inf with 0 for unreachable nodes: those nodes are never
	// popped by Dijkstra (they keep dist=inf there too), so their
	// potential value is irrelevant for rc evaluation. Zero keeps the
	// arithmetic safe and avoids a sentinel-aware Dijkstra path.
	for i := range dist {
		if dist[i] >= inf {
			dist[i] = 0
		}
	}
	return dist, nil
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
