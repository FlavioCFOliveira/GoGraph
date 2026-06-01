package adjlist_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestAdjList_TrivialShapes validates the directed adjlist backend
// against every degenerate shape in shapegen's trivial family. Each
// sub-test builds the shape, extracts the AdjList via lpg.Graph.AdjList(),
// and asserts the catalogue invariants documented in shapegen/trivial.go.
func TestAdjList_TrivialShapes(t *testing.T) {
	t.Parallel()

	t.Run("EmptyGraph", func(t *testing.T) {
		t.Parallel()
		g, err := shapegen.EmptyGraph().Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		if got := a.Order(); got != 0 {
			t.Errorf("Order = %d, want 0", got)
		}
		if got := a.Size(); got != 0 {
			t.Errorf("Size = %d, want 0", got)
		}
	})

	t.Run("SingleNode", func(t *testing.T) {
		t.Parallel()
		g, err := shapegen.SingleNode().Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		if got := a.Order(); got != 1 {
			t.Errorf("Order = %d, want 1", got)
		}
		if got := a.Size(); got != 0 {
			t.Errorf("Size = %d, want 0", got)
		}
	})

	t.Run("SingleEdge_directed", func(t *testing.T) {
		t.Parallel()
		// directed=true, weighted=false, selfLoop=false → K2 directed
		g, err := shapegen.SingleEdge(true, false, false).Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		if got := a.Order(); got != 2 {
			t.Errorf("Order = %d, want 2", got)
		}
		if got := a.Size(); got != 1 {
			t.Errorf("Size = %d, want 1", got)
		}
		if !a.HasEdge(0, 1) {
			t.Error("HasEdge(0,1) = false, want true")
		}
		if a.HasEdge(1, 0) {
			t.Error("HasEdge(1,0) = true, want false (directed)")
		}
	})

	t.Run("SingleEdge_undirected", func(t *testing.T) {
		t.Parallel()
		// directed=false, weighted=false, selfLoop=false → K2 undirected
		g, err := shapegen.SingleEdge(false, false, false).Build(adjlist.Config{Directed: false})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		if got := a.Order(); got != 2 {
			t.Errorf("Order = %d, want 2", got)
		}
		if got := a.Size(); got != 1 {
			t.Errorf("Size = %d, want 1", got)
		}
		if !a.HasEdge(0, 1) {
			t.Error("HasEdge(0,1) = false, want true")
		}
		if !a.HasEdge(1, 0) {
			t.Error("HasEdge(1,0) = false, want true (undirected mirror)")
		}
	})

	t.Run("SingleEdge_selfloop", func(t *testing.T) {
		t.Parallel()
		// directed=true, weighted=false, selfLoop=true
		g, err := shapegen.SingleEdge(true, false, true).Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		a := g.AdjList()
		if got := a.Order(); got != 1 {
			t.Errorf("Order = %d, want 1", got)
		}
		if got := a.Size(); got != 1 {
			t.Errorf("Size = %d, want 1", got)
		}
		if !a.HasEdge(0, 0) {
			t.Error("HasEdge(0,0) = false, want true (self-loop)")
		}
	})

	t.Run("ParallelDigon_explicit", func(t *testing.T) {
		t.Parallel()
		// Explicit sweep k=1..5: verify Size==k and directional invariants.
		for k := 1; k <= 5; k++ {
			k := k
			t.Run("", func(t *testing.T) {
				t.Parallel()
				// ParallelDigon forces Directed=true, Multigraph=true.
				g, err := shapegen.ParallelDigon(k).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("ParallelDigon(%d) Build: %v", k, err)
				}
				a := g.AdjList()
				if got := a.Order(); got != 2 {
					t.Errorf("k=%d: Order = %d, want 2", k, got)
				}
				if got := a.Size(); got != uint64(k) {
					t.Errorf("k=%d: Size = %d, want %d", k, got, k)
				}
				if !a.HasEdge(0, 1) {
					t.Errorf("k=%d: HasEdge(0,1) = false, want true", k)
				}
				if a.HasEdge(1, 0) {
					t.Errorf("k=%d: HasEdge(1,0) = true, want false (directed)", k)
				}
			})
		}
	})

	t.Run("ParallelDigon_rapid", func(t *testing.T) {
		t.Parallel()
		// Property sweep: for any k in [1,1000] the invariants hold.
		rapid.Check(t, func(rt *rapid.T) {
			k := rapid.IntRange(1, 1000).Draw(rt, "k")
			g, err := shapegen.ParallelDigon(k).Build(adjlist.Config{})
			if err != nil {
				rt.Fatalf("ParallelDigon(%d) Build: %v", k, err)
			}
			a := g.AdjList()
			if got := a.Order(); got != 2 {
				rt.Errorf("k=%d: Order = %d, want 2", k, got)
			}
			if got := a.Size(); got != uint64(k) {
				rt.Errorf("k=%d: Size = %d, want %d", k, got, k)
			}
			if !a.HasEdge(0, 1) {
				rt.Errorf("k=%d: HasEdge(0,1) = false, want true", k)
			}
			if a.HasEdge(1, 0) {
				rt.Errorf("k=%d: HasEdge(1,0) = true, want false", k)
			}
		})
	})

	t.Run("IsolatedOnly", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{0, 1, 5, 10} {
			n := n
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.IsolatedOnly(n).Build(adjlist.Config{Directed: true})
				if err != nil {
					t.Fatalf("IsolatedOnly(%d) Build: %v", n, err)
				}
				a := g.AdjList()
				if got := a.Order(); got != uint64(n) {
					t.Errorf("n=%d: Order = %d, want %d", n, got, n)
				}
				if got := a.Size(); got != 0 {
					t.Errorf("n=%d: Size = %d, want 0", n, got)
				}
				for i := 0; i < n; i++ {
					for j := 0; j < n; j++ {
						if a.HasEdge(i, j) {
							t.Errorf("n=%d: HasEdge(%d,%d) = true, want false (isolated nodes)", n, i, j)
						}
					}
				}
			})
		}
	})

	t.Run("UniversalSelfLoops", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{1, 4, 8} {
			n := n
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.UniversalSelfLoops(n, false).Build(adjlist.Config{Directed: true})
				if err != nil {
					t.Fatalf("UniversalSelfLoops(%d) Build: %v", n, err)
				}
				a := g.AdjList()
				if got := a.Order(); got != uint64(n) {
					t.Errorf("n=%d: Order = %d, want %d", n, got, n)
				}
				if got := a.Size(); got != uint64(n) {
					t.Errorf("n=%d: Size = %d, want %d", n, got, n)
				}
				for v := 0; v < n; v++ {
					if !a.HasEdge(v, v) {
						t.Errorf("n=%d: HasEdge(%d,%d) = false, want true (self-loop)", n, v, v)
					}
					// No cross-edges.
					for u := 0; u < n; u++ {
						if u != v && a.HasEdge(v, u) {
							t.Errorf("n=%d: HasEdge(%d,%d) = true, want false (no cross-edges)", n, v, u)
						}
					}
				}
			})
		}
	})
}
