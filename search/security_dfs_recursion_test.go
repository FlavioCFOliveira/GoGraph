package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// security_dfs_recursion_test.go is part of the GoGraph security test
// battery. It is a DEFENSE lock-in proving that the DFS-family
// algorithms that document an explicit work-stack ([TarjanSCC],
// [HopcroftTarjanBCC]) traverse a very long chain without overflowing
// the Go goroutine stack. A naively recursive implementation would
// recurse to depth O(V) and crash with a non-recoverable stack overflow
// — a denial-of-service vector on attacker-shaped deep graphs.
//
// [DFS] itself already has a P_1M soak guard (dfs_pn_test.go); this file
// extends the same protection to Tarjan SCC and biconnected-components,
// the two other deep-DFS algorithms. The fast short-layer cases run a
// chain large enough to overflow a recursive DFS (deep enough that a
// per-frame cost of a few hundred bytes would exceed the default 8 KB
// initial / 1 GB max goroutine stack only via the iterative design's
// heap-backed stack) while staying well under a second; the soak case
// scales to 1,000,000.

// secBuildDirectedChainCSR builds a directed path 0→1→…→(n-1) and returns
// its CSR plus the backing adjlist (for NodeID resolution). It reuses the
// shapegen Path generator that the existing DFS soak test relies on.
func secBuildDirectedChainCSR(tb testing.TB, n int) (*csr.CSR[int64], *adjlist.AdjList[int, int64]) {
	tb.Helper()
	g, err := shapegen.Path(n, true).Build(adjlist.Config{Directed: true})
	if err != nil {
		tb.Fatalf("shapegen.Path(%d): %v", n, err)
	}
	a := g.AdjList()
	return csr.BuildFromAdjList(a), a
}

// secBuildUndirectedChainCSR builds an undirected path 0—1—…—(n-1), the
// canonical deep input for biconnected-component decomposition: every
// internal vertex is an articulation point and every edge is its own
// biconnected component, so BCC must walk the full depth.
func secBuildUndirectedChainCSR(tb testing.TB, n int) *csr.CSR[int64] {
	tb.Helper()
	g, err := shapegen.Path(n, false).Build(adjlist.Config{Directed: false})
	if err != nil {
		tb.Fatalf("shapegen.Path(%d, undirected): %v", n, err)
	}
	return csr.BuildFromAdjList(g.AdjList())
}

// TestSec_Core_TarjanSCCDeepChainNoStackOverflow runs Tarjan SCC on a
// long directed chain. Each vertex is its own SCC, so the algorithm must
// descend the full chain depth. The iterative work-stack design must
// complete without a stack overflow and report exactly n singleton SCCs.
func TestSec_Core_TarjanSCCDeepChainNoStackOverflow(t *testing.T) {
	t.Parallel()

	const n = 200_000 // deep enough to crash a recursive DFS; runs in well under 1s.
	c, _ := secBuildDirectedChainCSR(t, n)

	sccs := TarjanSCC(c)
	if len(sccs) != n {
		t.Fatalf("TarjanSCC on directed chain P_%d: got %d SCCs, want %d singletons",
			n, len(sccs), n)
	}
	for _, comp := range sccs {
		if len(comp) != 1 {
			t.Fatalf("expected every SCC of a directed chain to be a singleton, got size %d", len(comp))
		}
	}
}

// TestSec_Core_BCCDeepChainNoStackOverflow runs biconnected-component
// decomposition on a long undirected chain. Each edge is its own
// biconnected component, forcing the explicit-stack DFS to the full
// depth. It must complete without a stack overflow.
func TestSec_Core_BCCDeepChainNoStackOverflow(t *testing.T) {
	t.Parallel()

	const n = 200_000
	c := secBuildUndirectedChainCSR(t, n)

	res := HopcroftTarjanBCC(c)
	// An undirected path P_n has n-1 edges, each a separate biconnected
	// component, and n-2 internal articulation points.
	if got := len(res.Components); got != n-1 {
		t.Fatalf("BCC on undirected chain P_%d: got %d components, want %d",
			n, got, n-1)
	}
}

// TestSec_Core_TarjanSCCDeepChainSoak scales the Tarjan stack-overflow
// guard to 1,000,000 vertices under the soak layer, matching the DFS
// P_1M soak guard's magnitude.
func TestSec_Core_TarjanSCCDeepChainSoak(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const n = 1_000_000
	c, _ := secBuildDirectedChainCSR(t, n)

	sccs := TarjanSCC(c)
	if len(sccs) != n {
		t.Fatalf("TarjanSCC on P_%d: got %d SCCs, want %d", n, len(sccs), n)
	}
}

// TestSec_Core_BCCDeepChainSoak scales the BCC stack-overflow guard to
// 1,000,000 vertices under the soak layer.
func TestSec_Core_BCCDeepChainSoak(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const n = 1_000_000
	c := secBuildUndirectedChainCSR(t, n)

	res := HopcroftTarjanBCC(c)
	if got := len(res.Components); got != n-1 {
		t.Fatalf("BCC on P_%d: got %d components, want %d", n, got, n-1)
	}
}
