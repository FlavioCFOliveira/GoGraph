package flow

import (
	"context"
	"errors"
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

// TestMinCostMaxFlow_NegativeArcs covers the Bellman-Ford bootstrap
// path on a small profit-bearing network. The cheap route
// 0->1->3 has cost -3+1 = -2 per unit (carries negative cost); the
// fallback route 0->2->3 has cost 1+1 = 2 per unit. SSP must push 5
// units along the cheap path before exhausting it and switching to
// the fallback for the remaining 10 units.
func TestMinCostMaxFlow_NegativeArcs(t *testing.T) {
	t.Parallel()
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 5, -3) // profit arc
	g.AddCostEdge(1, 3, 5, 1)
	g.AddCostEdge(0, 2, 10, 1)
	g.AddCostEdge(2, 3, 10, 1)

	flow, cost, err := MinCostMaxFlowCtx(context.Background(), g, 0, 3)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	const wantFlow = 15
	const wantCost = -10 + 20 // 5 * (-2)  +  10 * 2
	if flow != wantFlow {
		t.Fatalf("flow = %d, want %d", flow, wantFlow)
	}
	if cost != wantCost {
		t.Fatalf("cost = %d, want %d", cost, wantCost)
	}
}

// TestMinCostMaxFlow_NegativeArcs_NonContextEntry asserts that the
// non-context wrapper still returns a correct (flow, cost) on the
// same network — the wrapper must dispatch through the BF bootstrap
// transparently.
func TestMinCostMaxFlow_NegativeArcs_NonContextEntry(t *testing.T) {
	t.Parallel()
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 5, -3)
	g.AddCostEdge(1, 3, 5, 1)
	g.AddCostEdge(0, 2, 10, 1)
	g.AddCostEdge(2, 3, 10, 1)

	flow, cost := MinCostMaxFlow(g, 0, 3)
	if flow != 15 || cost != 10 {
		t.Fatalf("flow=%d cost=%d, want 15/10", flow, cost)
	}
}

// TestMinCostMaxFlow_NegativeCycle constructs a small network whose
// residual graph contains a negative cycle reachable from src; the
// bootstrap must surface this via ErrNegativeCycle without
// augmenting flow.
func TestMinCostMaxFlow_NegativeCycle(t *testing.T) {
	t.Parallel()
	// 0 -> 1 -> 2 -> 0 with costs that form a negative loop, plus a
	// 0 -> 3 (sink) escape arc.
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 5, -2)
	g.AddCostEdge(1, 2, 5, -2)
	g.AddCostEdge(2, 0, 5, -2) // closes the loop, total cost -6
	g.AddCostEdge(0, 3, 5, 1)

	flow, cost, err := MinCostMaxFlowCtx(context.Background(), g, 0, 3)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("err = %v, want ErrNegativeCycle", err)
	}
	if flow != 0 || cost != 0 {
		t.Fatalf("flow=%d cost=%d, want 0/0 on negative cycle", flow, cost)
	}
}
