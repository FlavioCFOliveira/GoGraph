package flow

import (
	"math/rand/v2"
	"testing"
)

func TestMinCostMaxFlow_Simple(t *testing.T) {
	t.Parallel()
	// Two parallel paths src -> mid -> sink with different costs:
	// Path A: 0->1->3 capacity 2 each, cost 1 each.
	// Path B: 0->2->3 capacity 2 each, cost 5 each.
	// Pushing 4 units of flow takes path A (cost 1+1=2 per unit)
	// for the first 2 units, then path B (cost 5+5=10) for the rest.
	// Total flow = 4, total cost = 2*2 + 2*10 = 4 + 20 = 24.
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 2, 1)
	g.AddCostEdge(1, 3, 2, 1)
	g.AddCostEdge(0, 2, 2, 5)
	g.AddCostEdge(2, 3, 2, 5)
	flow, cost := MinCostMaxFlow(g, 0, 3)
	if flow != 4 {
		t.Fatalf("flow = %d, want 4", flow)
	}
	if cost != 24 {
		t.Fatalf("cost = %d, want 24", cost)
	}
}

func TestMinCostMaxFlow_NoPath(t *testing.T) {
	t.Parallel()
	g := NewCostNetwork(3)
	g.AddCostEdge(0, 1, 10, 5)
	flow, cost := MinCostMaxFlow(g, 0, 2)
	if flow != 0 || cost != 0 {
		t.Fatalf("flow=%d cost=%d, want 0/0", flow, cost)
	}
}

// TestMinCostMaxFlow_FlowEqualsMaxFlow asserts the flow magnitude
// returned by MinCostMaxFlow equals the magnitude returned by Dinic
// MaxFlow on the same capacity structure (costs do not affect the
// maximum-flow value, only which augmenting paths are picked).
func TestMinCostMaxFlow_FlowEqualsMaxFlow(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(239, 241)) //nolint:gosec // deterministic
	for seed := 0; seed < 8; seed++ {
		const n = 10
		cn := NewCostNetwork(n)
		fn := NewNetwork(n)
		for i := 0; i < 4*n; i++ {
			u := r.IntN(n)
			v := r.IntN(n)
			capVal := r.IntN(10) + 1
			costVal := r.IntN(10) + 1
			if u == v {
				continue
			}
			cn.AddCostEdge(u, v, capVal, costVal)
			fn.AddEdge(u, v, capVal)
		}
		mcmfFlow, _ := MinCostMaxFlow(cn, 0, n-1)
		mfFlow := MaxFlow(fn, 0, n-1)
		if mcmfFlow != mfFlow {
			t.Fatalf("seed=%d: MinCostMaxFlow flow=%d, Dinic flow=%d", seed, mcmfFlow, mfFlow)
		}
	}
}
