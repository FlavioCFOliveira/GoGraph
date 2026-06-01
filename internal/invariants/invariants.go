// Package invariants provides hardened assertion helpers for graph
// property-based tests. Each helper takes a *lpg.Graph (or related
// type), checks a structural invariant, and calls t.Errorf (not
// t.Fatalf) so a single test can validate multiple invariants and
// accumulate all failures in one pass.
//
// Failure messages always include a counter-example — an offending
// vertex pair, component ID, or edge — so the caller can reproduce
// the failure from the log without a debugger.
//
// # Concurrency
//
// These helpers are NOT safe for concurrent use with mutations on the
// same graph. They are intended for use after a graph is fully
// constructed and before it is modified again.
package invariants

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// graphView is the minimal interface invariant helpers operate on.
// *lpg.Graph[N,W] satisfies it via AdjList().
type graphView[N comparable, W any] interface {
	AdjList() *adjlist.AdjList[N, W]
}

// AssertConnected fails t when the underlying undirected view of g
// has more than one weakly connected component. On failure the
// message states the number of components found.
//
// An empty graph (Order = 0) is considered vacuously connected.
func AssertConnected[N comparable, W any](t testing.TB, g graphView[N, W]) {
	t.Helper()
	adj := g.AdjList()
	if adj.Order() == 0 {
		return
	}
	c := csr.BuildFromAdjList(adj)
	_, k, err := search.WCC(c)
	if err != nil {
		t.Errorf("invariants.AssertConnected: WCC failed: %v", err)
		return
	}
	if k != 1 {
		t.Errorf("invariants.AssertConnected: graph has %d weakly connected components, want 1", k)
	}
}

// AssertDAG fails t when g contains a directed cycle (i.e. when any
// strongly connected component has size > 1, or a single-node SCC
// carries a self-loop). On failure it prints the first offending SCC
// (up to 5 nodes) as a counter-example.
func AssertDAG[N comparable, W any](t testing.TB, g graphView[N, W]) {
	t.Helper()
	adj := g.AdjList()
	c := csr.BuildFromAdjList(adj)
	sccs := search.TarjanSCC(c)
	m := adj.Mapper()

	for _, scc := range sccs {
		if len(scc) == 1 {
			// A single-node SCC is a DAG violation only if a self-loop
			// exists (u → u).
			id := scc[0]
			n, _ := m.Resolve(id)
			for nb := range adj.Neighbours(n) {
				if nb == n {
					t.Errorf("invariants.AssertDAG: self-loop on node %v (NodeID %d)", n, id)
					return
				}
			}
			continue
		}
		// Multi-node SCC — build a human-readable summary.
		labels := make([]string, 0, min(len(scc), 5))
		for _, id := range scc {
			if len(labels) >= 5 {
				labels = append(labels, "…")
				break
			}
			n, _ := m.Resolve(id)
			labels = append(labels, fmt.Sprintf("%v", n))
		}
		t.Errorf("invariants.AssertDAG: directed cycle detected; SCC of size %d: [%s]",
			len(scc), strings.Join(labels, ", "))
		return
	}
}

// AssertBipartite fails t when g is not 2-colourable. It performs a
// BFS 2-colouring over the undirected view of g (ignoring edge
// direction and weight). On failure the message identifies the
// offending edge (u, v) where both endpoints received the same colour.
func AssertBipartite[N comparable, W any](t testing.TB, g graphView[N, W]) {
	t.Helper()
	adj := g.AdjList()
	if adj.Order() == 0 {
		return
	}

	const (
		uncoloured uint8 = 0
		colourA    uint8 = 1
		colourB    uint8 = 2
	)

	m := adj.Mapper()
	maxID := uint64(m.MaxNodeID()) + 1
	colour := make([]uint8, maxID)

	var offendingU, offendingV N
	found := false

	m.Walk(func(id graph.NodeID, _ N) bool {
		if found {
			return false
		}
		if colour[id] != uncoloured {
			return true
		}
		// BFS from this seed.
		colour[id] = colourA
		queue := []graph.NodeID{id}
		for len(queue) > 0 && !found {
			u := queue[0]
			queue = queue[1:]
			uNode, _ := m.Resolve(u)
			nextColour := colourB
			if colour[u] == colourB {
				nextColour = colourA
			}
			for v := range adj.Neighbours(uNode) {
				vID, _ := m.Lookup(v)
				if colour[vID] == uncoloured {
					colour[vID] = nextColour
					queue = append(queue, vID)
				} else if colour[vID] == colour[u] {
					offendingU, offendingV = uNode, v
					found = true
					break
				}
			}
		}
		return !found
	})

	if found {
		t.Errorf("invariants.AssertBipartite: odd cycle; edge (%v → %v) has same colour", offendingU, offendingV)
	}
}

// AssertDistanceBound fails t when any BFS hop-distance exceeds the
// corresponding Dijkstra weighted distance. This verifies the
// well-known property: on a graph with unit or positive weights,
// BFS hop-count ≤ weighted shortest-path distance.
//
// bfsDepths is a map from NodeID to BFS depth produced by a call to
// [BuildBFSDepths]. dijkstra is the [search.Distances] result returned
// by [search.Dijkstra] from the same source node.
func AssertDistanceBound[W search.Weight](
	t testing.TB,
	bfsDepths map[graph.NodeID]int,
	dijkstra *search.Distances[W],
) {
	t.Helper()
	for nodeID, depth := range bfsDepths {
		djDist, reachable := dijkstra.Distance(nodeID)
		if !reachable {
			t.Errorf("invariants.AssertDistanceBound: node %d reachable by BFS (depth %d) but not by Dijkstra",
				nodeID, depth)
			continue
		}
		if toFloat64(djDist) < float64(depth) {
			t.Errorf("invariants.AssertDistanceBound: node %d: BFS depth %d > Dijkstra distance %v",
				nodeID, depth, djDist)
		}
	}
}

// toFloat64 converts a Weight to float64 for distance comparison.
// Integer depths in property-based tests fit safely in float64
// (< 2^53 nodes).
func toFloat64[W search.Weight](v W) float64 {
	switch x := any(v).(type) {
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case float64:
		return x
	default:
		return 0
	}
}

// AssertShapeEqual fails t when graphs a and b do not have the same
// Order (vertex count), Size (edge count), or edge set. On failure
// the message identifies the first discrepancy found. Edge weights
// are intentionally ignored; only topology is compared.
func AssertShapeEqual[N comparable, W any](t testing.TB, a, b *lpg.Graph[N, W]) {
	t.Helper()
	aa, ba := a.AdjList(), b.AdjList()

	if aa.Order() != ba.Order() {
		t.Errorf("invariants.AssertShapeEqual: Order mismatch: a=%d b=%d",
			aa.Order(), ba.Order())
		return
	}
	if aa.Size() != ba.Size() {
		t.Errorf("invariants.AssertShapeEqual: Size mismatch: a=%d b=%d",
			aa.Size(), ba.Size())
		return
	}

	// Every edge in a must exist in b.
	aa.Mapper().Walk(func(_ graph.NodeID, n N) bool {
		for nb := range aa.Neighbours(n) {
			if !ba.HasEdge(n, nb) {
				t.Errorf("invariants.AssertShapeEqual: edge (%v → %v) present in a but missing from b",
					n, nb)
				return false
			}
		}
		return true
	})
}

// BuildBFSDepths runs BFS from src on c and returns a map from
// NodeID to BFS depth (number of hops). This is a convenience helper
// for preparing the bfsDepths argument of [AssertDistanceBound].
func BuildBFSDepths[W any](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (map[graph.NodeID]int, error) {
	depths := make(map[graph.NodeID]int)
	if err := search.BFSCtx(ctx, c, src, func(node graph.NodeID, depth int) bool {
		depths[node] = depth
		return true
	}); err != nil {
		return nil, err
	}
	return depths, nil
}
