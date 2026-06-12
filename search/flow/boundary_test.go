package flow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clrsNetwork returns a fresh copy of the CLRS Fig. 26.1(a) network
// (max-flow s=0 → t=5 is 23) for use as the "valid call" oracle.
func clrsNetwork() *Network {
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

// clrsCostNetwork returns a two-path cost network:
//
//	0→1→3 (cap=1, cost=1+1) and 0→2→3 (cap=1, cost=2+2)
//
// Expected result: flow=2, cost=6.
func clrsCostNetwork() *CostNetwork {
	g := NewCostNetwork(4)
	g.AddCostEdge(0, 1, 1, 1)
	g.AddCostEdge(1, 3, 1, 1)
	g.AddCostEdge(0, 2, 1, 2)
	g.AddCostEdge(2, 3, 1, 2)
	return g
}

// TestBoundary_SrcEqSink_NoHang verifies that src==sink returns
// ErrInvalidEndpoints immediately without hanging.
//
// Each sub-test uses context.WithTimeout so the test binary reports a
// failure rather than stalling the suite if the bug is reintroduced.
//
// Fail-pre:  MaxFlow(g,1,1) hangs >5 s (infinite Dinic phase loop).
// Pass-post: all four ctx-aware entry points return ErrInvalidEndpoints
//
//	within the timeout.
func TestBoundary_SrcEqSink_NoHang(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond
	g := clrsNetwork()
	cg := clrsCostNetwork()

	cases := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{
			name: "MaxFlowCtx",
			run: func(ctx context.Context) error {
				_, err := MaxFlowCtx(ctx, g, 1, 1)
				return err
			},
		},
		{
			name: "EdmondsKarpCtx",
			run: func(ctx context.Context) error {
				_, err := EdmondsKarpCtx(ctx, g, 1, 1)
				return err
			},
		},
		{
			name: "PushRelabelMaxFlowCtx",
			run: func(ctx context.Context) error {
				_, err := PushRelabelMaxFlowCtx(ctx, g, 1, 1)
				return err
			},
		},
		{
			name: "MinCostMaxFlowCtx",
			run: func(ctx context.Context) error {
				_, _, err := MinCostMaxFlowCtx(ctx, cg, 1, 1)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			err := tc.run(ctx)
			if !errors.Is(err, ErrInvalidEndpoints) {
				t.Fatalf("%s(src==sink): err = %v, want ErrInvalidEndpoints", tc.name, err)
			}
		})
	}
}

// TestBoundary_SrcNegative verifies that a negative src returns
// ErrInvalidEndpoints without panicking.
//
// Fail-pre:  index-out-of-range panic.
// Pass-post: ErrInvalidEndpoints returned.
func TestBoundary_SrcNegative(t *testing.T) {
	t.Parallel()

	g := clrsNetwork()
	cg := clrsCostNetwork()

	t.Run("MaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, err := MaxFlowCtx(context.Background(), g, -1, 5)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("EdmondsKarpCtx", func(t *testing.T) {
		t.Parallel()
		_, err := EdmondsKarpCtx(context.Background(), g, -1, 5)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("PushRelabelMaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, err := PushRelabelMaxFlowCtx(context.Background(), g, -1, 5)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("MinCostMaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, _, err := MinCostMaxFlowCtx(context.Background(), cg, -1, 3)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
}

// TestBoundary_SinkOutOfRange verifies that sink >= g.N() returns
// ErrInvalidEndpoints without panicking.
//
// Fail-pre:  index-out-of-range panic.
// Pass-post: ErrInvalidEndpoints returned.
func TestBoundary_SinkOutOfRange(t *testing.T) {
	t.Parallel()

	g := clrsNetwork()      // N() == 6
	cg := clrsCostNetwork() // N() == 4

	t.Run("MaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, err := MaxFlowCtx(context.Background(), g, 0, 7)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("EdmondsKarpCtx", func(t *testing.T) {
		t.Parallel()
		_, err := EdmondsKarpCtx(context.Background(), g, 0, 7)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("PushRelabelMaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, err := PushRelabelMaxFlowCtx(context.Background(), g, 0, 7)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
	t.Run("MinCostMaxFlowCtx", func(t *testing.T) {
		t.Parallel()
		_, _, err := MinCostMaxFlowCtx(context.Background(), cg, 0, 7)
		if !errors.Is(err, ErrInvalidEndpoints) {
			t.Fatalf("err = %v, want ErrInvalidEndpoints", err)
		}
	})
}

// TestBoundary_ValidCallsUnaffected confirms that valid (src != sink,
// in-range) calls still produce correct results after the fix.
func TestBoundary_ValidCallsUnaffected(t *testing.T) {
	t.Parallel()

	t.Run("MaxFlow", func(t *testing.T) {
		t.Parallel()
		if got := MaxFlow(clrsNetwork(), 0, 5); got != 23 {
			t.Fatalf("MaxFlow = %d, want 23", got)
		}
	})
	t.Run("EdmondsKarp", func(t *testing.T) {
		t.Parallel()
		if got := EdmondsKarp(clrsNetwork(), 0, 5); got != 23 {
			t.Fatalf("EdmondsKarp = %d, want 23", got)
		}
	})
	t.Run("PushRelabelMaxFlow", func(t *testing.T) {
		t.Parallel()
		if got := PushRelabelMaxFlow(clrsNetwork(), 0, 5); got != 23 {
			t.Fatalf("PushRelabelMaxFlow = %d, want 23", got)
		}
	})
	t.Run("MinCostMaxFlow", func(t *testing.T) {
		t.Parallel()
		if f, c := MinCostMaxFlow(clrsCostNetwork(), 0, 3); f != 2 || c != 6 {
			t.Fatalf("MinCostMaxFlow = (%d, %d), want (2, 6)", f, c)
		}
	})
}
