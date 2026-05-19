package flow

import (
	"math/rand/v2"
	"testing"
)

func TestEdmondsKarp_CLRS(t *testing.T) {
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
	if got := EdmondsKarp(g, 0, 5); got != 23 {
		t.Fatalf("EdmondsKarp = %d, want 23", got)
	}
}

func TestEdmondsKarp_NoPath(t *testing.T) {
	t.Parallel()
	g := NewNetwork(3)
	g.AddEdge(0, 1, 10)
	if got := EdmondsKarp(g, 0, 2); got != 0 {
		t.Fatalf("EdmondsKarp = %d, want 0 when sink unreachable", got)
	}
}

// TestEdmondsKarp_AgreesWithDinic asserts the two max-flow
// implementations produce identical totals on random networks — the
// max-flow value is unique even when the witness flow is not.
func TestEdmondsKarp_AgreesWithDinic(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(109, 113)) //nolint:gosec // deterministic
	for seed := 0; seed < 10; seed++ {
		const n = 16
		// Build two identical networks (Dinic mutates capacities).
		ek := NewNetwork(n)
		dn := NewNetwork(n)
		for i := 0; i < 4*n; i++ {
			u := r.IntN(n)
			v := r.IntN(n)
			capVal := r.IntN(20) + 1
			if u == v {
				continue
			}
			ek.AddEdge(u, v, capVal)
			dn.AddEdge(u, v, capVal)
		}
		ekFlow := EdmondsKarp(ek, 0, n-1)
		dnFlow := MaxFlow(dn, 0, n-1)
		if ekFlow != dnFlow {
			t.Fatalf("seed=%d: EdmondsKarp=%d Dinic=%d", seed, ekFlow, dnFlow)
		}
	}
}

// BenchmarkEdmondsKarp_RandomNetwork establishes the baseline cost
// against which the Dinic-based [MaxFlow] is expected to win on
// non-unit-capacity networks.
func BenchmarkEdmondsKarp_RandomNetwork(b *testing.B) {
	r := rand.New(rand.NewPCG(127, 131)) //nolint:gosec // deterministic
	const n = 256
	edges := make([][3]int, 0, 8*n)
	for i := 0; i < 8*n; i++ {
		u := r.IntN(n)
		v := r.IntN(n)
		if u == v {
			continue
		}
		edges = append(edges, [3]int{u, v, r.IntN(50) + 1})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := NewNetwork(n)
		for _, e := range edges {
			g.AddEdge(e[0], e[1], e[2])
		}
		_ = EdmondsKarp(g, 0, n-1)
	}
}
