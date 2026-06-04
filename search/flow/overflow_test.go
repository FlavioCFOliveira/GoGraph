package flow

import (
	"context"
	"errors"
	"testing"
)

// TestMaxFlow_ParallelInfCapacitiesRejected is the headline regression
// for the silent-overflow defect: two parallel s->t edges each at the
// inf sentinel (1<<62) would, before the boundary check, accumulate a
// total flow of 2*(1<<62) = 1<<63, wrapping int64 to a negative value.
//
// Fail-pre: MaxFlow returned a negative (wrapped) flow.
// Pass-post: MaxFlowCtx surfaces ErrCapacityOverflow and the
// non-context MaxFlow returns the safe 0 sentinel, never a negative.
func TestMaxFlow_ParallelInfCapacitiesRejected(t *testing.T) {
	t.Parallel()
	g := NewNetwork(2)
	g.AddEdge(0, 1, 1<<62)
	g.AddEdge(0, 1, 1<<62)

	got, err := MaxFlowCtx(context.Background(), g, 0, 1)
	if !errors.Is(err, ErrCapacityOverflow) {
		t.Fatalf("MaxFlowCtx err = %v, want ErrCapacityOverflow", err)
	}
	if got != 0 {
		t.Fatalf("MaxFlowCtx flow = %d, want 0 on overflow", got)
	}
	if simple := MaxFlow(g, 0, 1); simple < 0 {
		t.Fatalf("MaxFlow = %d, must never return a negative/wrapped flow", simple)
	} else if simple != 0 {
		t.Fatalf("MaxFlow = %d, want 0 on overflow", simple)
	}
}

// TestMaxFlow_SourceCutSumOverflowRejected exercises the conservative
// source-cut bound: each edge is individually below the sentinel, but
// their sum out of the source exceeds it, so the running total could
// wrap. Both parallel-edge and distinct-neighbour shapes are covered.
func TestMaxFlow_SourceCutSumOverflowRejected(t *testing.T) {
	t.Parallel()
	const big = capInf - 1 // largest legal single capacity

	// Two parallel edges 0->1, each just below the sentinel: sum = 2*big.
	parallel := NewNetwork(2)
	parallel.AddEdge(0, 1, big)
	parallel.AddEdge(0, 1, big)

	// Two distinct neighbours from the source: sum out of src = 2*big.
	fanout := NewNetwork(3)
	fanout.AddEdge(0, 1, big)
	fanout.AddEdge(0, 2, big)
	fanout.AddEdge(1, 2, 1)

	for _, tc := range []struct {
		name string
		g    *Network
		sink int
	}{
		{"parallel", parallel, 1},
		{"fanout", fanout, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := MaxFlowCtx(context.Background(), tc.g, 0, tc.sink); !errors.Is(err, ErrCapacityOverflow) {
				t.Fatalf("MaxFlowCtx err = %v, want ErrCapacityOverflow", err)
			}
			if _, err := EdmondsKarpCtx(context.Background(), tc.g, 0, tc.sink); !errors.Is(err, ErrCapacityOverflow) {
				t.Fatalf("EdmondsKarpCtx err = %v, want ErrCapacityOverflow", err)
			}
			if _, err := PushRelabelMaxFlowCtx(context.Background(), tc.g, 0, tc.sink); !errors.Is(err, ErrCapacityOverflow) {
				t.Fatalf("PushRelabelMaxFlowCtx err = %v, want ErrCapacityOverflow", err)
			}
		})
	}
}

// TestMaxFlow_NegativeCapacityRejected guards the lower bound of the
// per-edge range check.
func TestMaxFlow_NegativeCapacityRejected(t *testing.T) {
	t.Parallel()
	g := NewNetwork(2)
	g.AddEdge(0, 1, -1)
	if _, err := MaxFlowCtx(context.Background(), g, 0, 1); !errors.Is(err, ErrCapacityOverflow) {
		t.Fatalf("MaxFlowCtx err = %v, want ErrCapacityOverflow for negative capacity", err)
	}
}

// TestMaxFlow_InRangeUnaffected confirms the boundary check is a pure
// guard: a legal CLRS-scale network still produces the exact same
// max-flow across all three engines (no regression for valid inputs).
func TestMaxFlow_InRangeUnaffected(t *testing.T) {
	t.Parallel()
	build := func() *Network {
		// CLRS Fig. 26.1(a): max flow s=0 -> t=5 is 23.
		g := NewNetwork(6)
		g.AddEdge(0, 1, 16)
		g.AddEdge(0, 2, 13)
		g.AddEdge(1, 2, 10)
		g.AddEdge(2, 1, 4)
		g.AddEdge(1, 3, 12)
		g.AddEdge(2, 4, 14)
		g.AddEdge(3, 2, 9)
		g.AddEdge(3, 5, 20)
		g.AddEdge(4, 3, 7)
		g.AddEdge(4, 5, 4)
		return g
	}
	if got := MaxFlow(build(), 0, 5); got != 23 {
		t.Fatalf("MaxFlow = %d, want 23", got)
	}
	if got := EdmondsKarp(build(), 0, 5); got != 23 {
		t.Fatalf("EdmondsKarp = %d, want 23", got)
	}
	if got := PushRelabelMaxFlow(build(), 0, 5); got != 23 {
		t.Fatalf("PushRelabelMaxFlow = %d, want 23", got)
	}
}

// TestMinCostMaxFlow_CostProductOverflowRejected exercises the cost
// bound of validateCostCapacities: capacities are legal and their sum
// out of the source is legal, but maxFlow * maxAbsCost overflows int64,
// so totalCost += push*cost would wrap. The flow itself stays in range,
// isolating the cost-product branch.
func TestMinCostMaxFlow_CostProductOverflowRejected(t *testing.T) {
	t.Parallel()
	// Source cut = 1<<40 with a per-unit cost of 1<<40: product = 1<<80,
	// far beyond int64, while every capacity stays below the sentinel.
	g := NewCostNetwork(2)
	g.AddCostEdge(0, 1, 1<<40, 1<<40)

	flow, cost, err := MinCostMaxFlowCtx(context.Background(), g, 0, 1)
	if !errors.Is(err, ErrCapacityOverflow) {
		t.Fatalf("MinCostMaxFlowCtx err = %v, want ErrCapacityOverflow", err)
	}
	if flow != 0 || cost != 0 {
		t.Fatalf("MinCostMaxFlowCtx = (%d, %d), want (0, 0) on overflow", flow, cost)
	}
	if f, c := MinCostMaxFlow(g, 0, 1); f != 0 || c != 0 {
		t.Fatalf("MinCostMaxFlow = (%d, %d), want (0, 0) on overflow", f, c)
	}
}

// TestMinCostMaxFlow_InRangeUnaffected confirms a legal cost-network
// still computes the correct (flow, cost) — the cost guard does not
// perturb valid inputs.
func TestMinCostMaxFlow_InRangeUnaffected(t *testing.T) {
	t.Parallel()
	// Two unit-capacity paths 0->1->3 (cost 1+1) and 0->2->3 (cost 2+2):
	// pushing both units costs 2 + 4 = 6 for a total flow of 2.
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 1, 1)
	g.AddCostEdge(1, 3, 1, 1)
	g.AddCostEdge(0, 2, 1, 2)
	g.AddCostEdge(2, 3, 1, 2)
	flow, cost := MinCostMaxFlow(g, 0, 3)
	if flow != 2 || cost != 6 {
		t.Fatalf("MinCostMaxFlow = (%d, %d), want (2, 6)", flow, cost)
	}
}
