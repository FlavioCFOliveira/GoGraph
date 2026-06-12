//go:build soak || nightly

package csr_test

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestCSR_MegaBuild_MaxBA exercises BuildFromAdjList on the largest
// graph that BarabasiAlbert permits (n=100_000, m0=2), which yields
// approximately 200_000 edges.  The original brief called for 1e7
// edges via n=5_000_000 but BarabasiAlbert panics for n > 100_000
// (see its constructor invariant); the maximum-allowed parameters are
// used instead.
//
// Catalogue invariant (BarabasiAlbert godoc):
//
//	Size() == m0*(m0-1)/2 + (n-m0)*m0
//	       == 1 + (100_000 - 2)*2 == 199_997  (undirected edges)
//
// The CSR snapshot stores both directed arcs for each undirected
// edge, so CSR.Size() == 2 * AdjList.Size().
//
// CI budget: 5 s.  The build is O(V+E) and runs in low single-digit
// milliseconds on a development machine; the generous ceiling absorbs
// cloud-runner variance while still catching O(V*E) regressions.
func TestCSR_MegaBuild_MaxBA(t *testing.T) {
	const budget = 5 * time.Second

	shape := shapegen.BarabasiAlbert(100_000, 2, 42)
	g, err := shape.Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	t.Logf("AdjList: Order=%d Size=%d", a.Order(), a.Size())

	start := time.Now()
	c := csr.BuildFromAdjList(a)
	elapsed := time.Since(start)

	t.Logf("BuildFromAdjList elapsed: %v", elapsed)

	if elapsed > budget {
		t.Errorf("BuildFromAdjList took %v, want < %v", elapsed, budget)
	}
	if c.Order() != a.Order() {
		t.Errorf("Order mismatch: csr=%d adjlist=%d", c.Order(), a.Order())
	}
	// Undirected AdjList stores each edge once; CSR mirrors both arcs.
	if c.Size() != 2*a.Size() {
		t.Errorf("Size mismatch: csr=%d want 2*adjlist=%d", c.Size(), 2*a.Size())
	}
}
