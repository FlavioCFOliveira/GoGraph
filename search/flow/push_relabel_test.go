package flow

import (
	"math/rand/v2"
	"testing"
)

func TestPushRelabelMaxFlow_CLRS(t *testing.T) {
	t.Parallel()
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
	if got := PushRelabelMaxFlow(g, 0, 5); got != 23 {
		t.Fatalf("PushRelabelMaxFlow = %d, want 23", got)
	}
}

func TestPushRelabelMaxFlow_NoPath(t *testing.T) {
	t.Parallel()
	g := NewNetwork(3)
	g.AddEdge(0, 1, 10)
	if got := PushRelabelMaxFlow(g, 0, 2); got != 0 {
		t.Fatalf("PushRelabelMaxFlow = %d, want 0 when sink unreachable", got)
	}
}

// TestPushRelabelMaxFlow_AgreesWithDinic fuzzes random networks and
// asserts the three implementations all report the same max-flow
// value.
func TestPushRelabelMaxFlow_AgreesWithDinic(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(229, 233)) //nolint:gosec // deterministic
	for seed := 0; seed < 10; seed++ {
		const n = 12
		// Build three identical networks (each algorithm mutates capacities).
		pr := NewNetwork(n)
		dn := NewNetwork(n)
		ek := NewNetwork(n)
		for i := 0; i < 4*n; i++ {
			u := r.IntN(n)
			v := r.IntN(n)
			c := r.IntN(10) + 1
			if u == v {
				continue
			}
			pr.AddEdge(u, v, c)
			dn.AddEdge(u, v, c)
			ek.AddEdge(u, v, c)
		}
		prFlow := PushRelabelMaxFlow(pr, 0, n-1)
		dnFlow := MaxFlow(dn, 0, n-1)
		ekFlow := EdmondsKarp(ek, 0, n-1)
		if prFlow != dnFlow || prFlow != ekFlow {
			t.Fatalf("seed=%d: PR=%d Dinic=%d EK=%d", seed, prFlow, dnFlow, ekFlow)
		}
	}
}
