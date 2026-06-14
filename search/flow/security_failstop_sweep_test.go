package flow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// security_failstop_sweep_test.go is part of the GoGraph security test
// battery. It is a DEFENSE lock-in for the flow package's "fail-stop,
// never fail-silent, never panic" contract on adversarial inputs.
//
// boundary_test.go already pins ErrInvalidEndpoints for individual entry
// points and overflow_test.go pins ErrCapacityOverflow for MaxFlow /
// MinCostMaxFlow. This file adds the cross-cutting guarantee a security
// battery needs: across EVERY context-aware entry point, an
// out-of-bounds / degenerate / overflowing network is rejected with a
// typed error and NEVER panics or hangs — closing the
// denial-of-service-via-panic surface uniformly. The non-context entry
// points must mirror this by returning the documented zero result
// instead of crashing.

// secValidNetwork is the CLRS Fig. 26.1(a) network (max-flow 0→5 == 23),
// reused as the "valid call still works" oracle.
func secValidNetwork() *Network {
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

// ctxFlowEntry names a context-aware max-flow entry point under test.
type ctxFlowEntry struct {
	name string
	call func(ctx context.Context, g *Network, src, sink int) (int, error)
}

func ctxFlowEntries() []ctxFlowEntry {
	return []ctxFlowEntry{
		{"MaxFlowCtx", MaxFlowCtx},
		{"EdmondsKarpCtx", EdmondsKarpCtx},
		{"PushRelabelMaxFlowCtx", PushRelabelMaxFlowCtx},
	}
}

// TestSec_Core_FlowInvalidEndpointsTypedErrorAllEntries asserts every
// context-aware max-flow entry point rejects out-of-range and
// src==sink endpoints with ErrInvalidEndpoints — not a panic, not a
// hang. Each call runs under a short context deadline so a reintroduced
// hang fails the test instead of stalling the suite.
func TestSec_Core_FlowInvalidEndpointsTypedErrorAllEntries(t *testing.T) {
	t.Parallel()

	bad := []struct {
		name      string
		src, sink int
	}{
		{"src_negative", -1, 5},
		{"sink_negative", 0, -3},
		{"src_oob", 6, 5},
		{"sink_oob", 0, 6},
		{"src_eq_sink", 2, 2},
		{"both_oob", 99, 100},
	}

	for _, entry := range ctxFlowEntries() {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()
			for _, b := range bad {
				b := b
				t.Run(b.name, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					flow, err := entry.call(ctx, secValidNetwork(), b.src, b.sink)
					if !errors.Is(err, ErrInvalidEndpoints) {
						t.Fatalf("%s(%d,%d): err = %v, want ErrInvalidEndpoints",
							entry.name, b.src, b.sink, err)
					}
					if flow != 0 {
						t.Fatalf("%s(%d,%d): flow = %d on rejected input, want 0",
							entry.name, b.src, b.sink, flow)
					}
				})
			}
		})
	}
}

// TestSec_Core_FlowCapacityOverflowTypedErrorAllEntries asserts every
// context-aware max-flow entry point rejects a network whose source-cut
// capacity overflows the int64 sentinel with ErrCapacityOverflow rather
// than returning a wrapped (negative) flow.
func TestSec_Core_FlowCapacityOverflowTypedErrorAllEntries(t *testing.T) {
	t.Parallel()

	// Two parallel edges out of the source, each just over half the
	// sentinel, so their sum crosses capInf and the source-cut bound trips.
	build := func() *Network {
		g := NewNetwork(3)
		g.AddEdge(0, 1, capInf/2+1)
		g.AddEdge(0, 2, capInf/2+1)
		g.AddEdge(1, 2, 1)
		return g
	}

	for _, entry := range ctxFlowEntries() {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			flow, err := entry.call(ctx, build(), 0, 2)
			if !errors.Is(err, ErrCapacityOverflow) {
				t.Fatalf("%s: err = %v, want ErrCapacityOverflow", entry.name, err)
			}
			if flow != 0 {
				t.Fatalf("%s: flow = %d on overflowing network, want 0", entry.name, flow)
			}
		})
	}
}

// TestSec_Core_FlowNonCtxReturnsZeroNeverPanics asserts the non-context
// entry points honour the documented contract: on invalid endpoints or
// overflow they return the zero result (0, or (0,0) for min-cost) and
// never panic. A panic here would be a denial-of-service surface for any
// caller that cannot supply a context.
func TestSec_Core_FlowNonCtxReturnsZeroNeverPanics(t *testing.T) {
	t.Parallel()

	// Invalid endpoints.
	if got := MaxFlow(secValidNetwork(), 2, 2); got != 0 {
		t.Fatalf("MaxFlow(src==sink) = %d, want 0", got)
	}
	if got := MaxFlow(secValidNetwork(), -1, 5); got != 0 {
		t.Fatalf("MaxFlow(src<0) = %d, want 0", got)
	}
	if got := MaxFlow(secValidNetwork(), 0, 99); got != 0 {
		t.Fatalf("MaxFlow(sink oob) = %d, want 0", got)
	}

	// Min-cost on invalid endpoints returns (0, 0).
	cn := NewCostNetwork(4)
	cn.AddCostEdge(0, 1, 1, 1)
	cn.AddCostEdge(1, 3, 1, 1)
	if f, c := MinCostMaxFlow(cn, 2, 2); f != 0 || c != 0 {
		t.Fatalf("MinCostMaxFlow(src==sink) = (%d,%d), want (0,0)", f, c)
	}
	if f, c := MinCostMaxFlow(cn, 0, 99); f != 0 || c != 0 {
		t.Fatalf("MinCostMaxFlow(sink oob) = (%d,%d), want (0,0)", f, c)
	}
}

// TestSec_Core_FlowValidCallUnaffected is the DEFENSE backstop: the
// adversarial guards above must not have narrowed correct behaviour —
// a well-formed network still computes the right max-flow on every entry.
func TestSec_Core_FlowValidCallUnaffected(t *testing.T) {
	t.Parallel()

	const want = 23 // CLRS Fig 26.1(a) max-flow 0→5.
	ctx := context.Background()
	for _, entry := range ctxFlowEntries() {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()
			got, err := entry.call(ctx, secValidNetwork(), 0, 5)
			if err != nil {
				t.Fatalf("%s: unexpected error %v", entry.name, err)
			}
			if got != want {
				t.Fatalf("%s: max-flow = %d, want %d", entry.name, got, want)
			}
		})
	}
}
