package flow

// push_relabel_ctx_cancel_test.go — sprint 349, rmp #2593.
//
// Regression test for the worst of the five cancellation defects audited for
// #2489: PushRelabelMaxFlowCtx returned the TRUE, COMPLETE maximum flow with a
// NIL error under an already-cancelled context, on every network below its
// 4096-discharge poll stride (measured blind at 8, 64 and 1536 vertices).
//
// Root cause: the FIFO queue loop incremented tick BEFORE testing
// tick&0xFFF == 0, so the counter was already 1 on the first iteration and the
// mask was first true only on the 4096th discharge. The correct idiom — check
// the mask, THEN increment, so the poll happens on iteration 0 — was already
// in the tree at search/dijkstra.go and search/prim.go.
//
// The failure this closes: a request-scoped deadline fires, the caller's
// context is dead, and the library computes the whole max flow anyway and
// reports success. No backpressure, no cancellation, unbounded work past the
// deadline, and nothing downstream able to detect either problem — a direct
// breach of the EXTREME / MASSIVE Concurrent Ready mandate's
// context-aware-blocking rule (CLAUDE.md).
//
// The fixture is 8 vertices, BELOW the stride, which is precisely the regime
// in which the entry point was blind. The instrument is a real pre-cancelled
// context.WithCancel. Cancellation is verified with errors.Is, never with
// err != nil: ErrInvalidEndpoints and ErrCapacityOverflow are both non-nil, so
// an err != nil oracle would score a validation refusal as a cancellation.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"testing"
)

// prCancelN is the fixture size: 8 vertices, below the 4096 poll stride.
const prCancelN = 8

// buildTwoChainNetwork returns an 8-vertex network made of two vertex-disjoint
// chains from src=0 to sink=7:
//
//	0 -3-> 1 -3-> 3 -3-> 5 -3-> 7   (bottleneck 3)
//	0 -2-> 2 -2-> 4 -2-> 6 -2-> 7   (bottleneck 2)
//
// The chains share no interior vertex, so the maximum flow is 3 + 2 = 5 by
// inspection. PushRelabelMaxFlow mutates the network in place, so each call
// needs a freshly built one.
func buildTwoChainNetwork() *Network {
	g := NewNetwork(prCancelN)
	g.AddEdge(0, 1, 3)
	g.AddEdge(1, 3, 3)
	g.AddEdge(3, 5, 3)
	g.AddEdge(5, 7, 3)
	g.AddEdge(0, 2, 2)
	g.AddEdge(2, 4, 2)
	g.AddEdge(4, 6, 2)
	g.AddEdge(6, 7, 2)
	return g
}

// TestPushRelabelMaxFlowCtx_CancelBelowStride pins that
// PushRelabelMaxFlowCtx reports cancellation on a network far below the
// 4096-discharge poll stride, and that the live-context answer is unchanged.
//
// RED before the fix: the cancelled arm returned (5, nil) — the true maximum
// flow with no error.
func TestPushRelabelMaxFlowCtx_CancelBelowStride(t *testing.T) {
	t.Parallel()

	const wantMax = 5

	// Live context: the answer must be unchanged.
	got, err := PushRelabelMaxFlowCtx(context.Background(), buildTwoChainNetwork(), 0, prCancelN-1)
	if err != nil {
		t.Fatalf("live PushRelabelMaxFlowCtx: %v", err)
	}
	if got != wantMax {
		t.Fatalf("live PushRelabelMaxFlowCtx = %d, want %d", got, wantMax)
	}

	// Cancelled context: cancellation must be reported.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("fixture context: Err() = %v, want context.Canceled", ctx.Err())
	}

	flowSoFar, err := PushRelabelMaxFlowCtx(ctx, buildTwoChainNetwork(), 0, prCancelN-1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PushRelabelMaxFlowCtx: err = %v, want context.Canceled (flow=%d)", err, flowSoFar)
	}
	// The documented contract on cancellation is (totalSoFar, ctx.Err()) where
	// totalSoFar is the excess accumulated at sink — a valid LOWER BOUND on
	// the true max flow. Never above it, and never negative.
	if flowSoFar < 0 || flowSoFar > wantMax {
		t.Errorf("cancelled PushRelabelMaxFlowCtx: flow = %d, want a lower bound in [0, %d]", flowSoFar, wantMax)
	}
}
