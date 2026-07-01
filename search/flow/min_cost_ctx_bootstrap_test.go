package flow

// Regression lock-in for the 2026-07-01 hostility+load audit finding F10
// (#1834): the Bellman-Ford potential bootstrap in MinCostMaxFlowCtx ran its
// O(V*E) relaxation passes with no ctx.Err() poll, so a context cancelled
// during the bootstrap on a large dense negative-cost network was not observed
// until the whole bootstrap completed. The fix polls ctx at each
// relaxation-pass boundary.
//
// This test calls the internal bellmanFordBootstrap directly with an
// already-cancelled context. It is deliberately non-vacuous: without the
// pass-boundary poll the bootstrap runs to completion and returns a valid
// potential vector with a nil error, so a result-level test on
// MinCostMaxFlowCtx would be caught by the downstream SSP-loop poll regardless
// and prove nothing. Calling the bootstrap directly pins that the bootstrap
// itself now honours cancellation and returns context.Canceled without
// finishing its passes.

import (
	"context"
	"errors"
	"testing"
)

func TestMinCostMaxFlow_BootstrapHonoursCtxCancel(t *testing.T) {
	// A negative-cost arc guarantees the bootstrap is the path under test
	// (matching hasNegativeCost); the extra arcs give the relaxation real work.
	const n = 64
	g := NewCostNetwork(n)
	g.AddCostEdge(0, 1, 5, -3)
	for u := 1; u < n-1; u++ {
		g.AddCostEdge(u, u+1, 5, 1)
		g.AddCostEdge(0, u, 5, 2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pot, err := bellmanFordBootstrap(ctx, g, 0)
	if err == nil {
		t.Fatalf("bellmanFordBootstrap ignored a cancelled context and returned a potential vector (len=%d); want context.Canceled", len(pot))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("bellmanFordBootstrap error = %v; want errors.Is(context.Canceled)", err)
	}
}
