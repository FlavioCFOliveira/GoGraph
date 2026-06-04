package lpg

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestIsolation_EdgeInstanceStores_CrossStoreRequiresView characterises the
// documented cross-store consistency contract of the per-instance edge
// metadata stores (#1284). [Graph.EdgeCreateCount] and the per-instance
// surfaces [Graph.EdgeLabelsAt] / [Graph.EdgePropertiesAt] are each guarded by
// their own per-shard mutex and are only per-operation atomic; they are NOT
// cross-store consistent outside the transaction-visibility barrier. A reader
// that correlates the CREATE count with the number of populated per-instance
// property indices — WITHOUT [Graph.View] — can observe a multi-CREATE
// multigraph transaction half-applied (count already at 2 while only one
// instance is populated). The same correlation wrapped in [Graph.View] never
// observes that partial state because the writer holds the barrier for the
// whole apply.
//
// This is a CONTRACT/characterization test, not a bug fix: it locks the
// currently-documented opt-in behaviour described on those accessors and in
// docs/isolation-design.md. It proves both halves under a deterministic
// channel handshake (no flaky timing):
//
//   - the direct (no-View) correlation observes violation > 0, while
//   - the same correlation wrapped in [Graph.View] observes ZERO violations.
//
// The direct reader requests only the stores' own shard locks, never visMu, so
// the handshake cannot deadlock against the writer that holds visMu via
// [Graph.ApplyAtomically]. Run under -race: the per-shard locks make every
// access data-race-free; the gap proven OPEN here is the logical cross-store
// partial-transaction visibility, not a memory race.
func TestIsolation_EdgeInstanceStores_CrossStoreRequiresView(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}

	// countPopulatedInstances returns how many per-instance property indices in
	// [1, upTo] currently carry a property map for the directed edge (a, b).
	countPopulatedInstances := func(upTo int64) int64 {
		var n int64
		for idx := int64(1); idx <= upTo; idx++ {
			if len(g.EdgePropertiesAt("a", "b", idx)) > 0 {
				n++
			}
		}
		return n
	}

	// crossStoreViolated reports whether EdgeCreateCount disagrees with the
	// number of populated per-instance property indices — the partial
	// cross-store view the contract permits only outside View.
	crossStoreViolated := func() bool {
		c := g.EdgeCreateCount("a", "b")
		return c != countPopulatedInstances(c)
	}

	// applyTwoParallelEdges commits two parallel (a)-[:R]->(b) instances inside
	// one ApplyAtomically. beforeSecond, when non-nil, runs after the first
	// instance is fully populated and its count bumped but BEFORE the second
	// instance is populated — the window in which the count leads the populated
	// indices. The instance index is the 1-based value IncEdgeCreateCount
	// returns, exactly as CreateRelationship wires it.
	applyTwoParallelEdges := func(beforeSecond func()) error {
		return g.ApplyAtomically(func() error {
			h1, err := g.AddEdgeH("a", "b", 0)
			if err != nil {
				return err
			}
			i1 := g.IncEdgeCreateCount("a", "b")
			g.SetEdgeLabelAt("a", "b", i1, "R")
			g.SetEdgePropertyAt("a", "b", i1, "seq", Int64Value(i1))
			g.SetEdgeLabelByHandle("a", "b", h1, "R")
			g.SetEdgePropertyByHandle("a", "b", h1, "seq", Int64Value(i1))

			if beforeSecond != nil {
				beforeSecond()
			}

			h2, err := g.AddEdgeH("a", "b", 0)
			if err != nil {
				return err
			}
			i2 := g.IncEdgeCreateCount("a", "b")
			g.SetEdgeLabelAt("a", "b", i2, "R")
			g.SetEdgePropertyAt("a", "b", i2, "seq", Int64Value(i2))
			g.SetEdgeLabelByHandle("a", "b", h2, "R")
			g.SetEdgePropertyByHandle("a", "b", h2, "seq", Int64Value(i2))
			return nil
		})
	}

	// Half 1 — direct correlation, NO View. The writer bumps the count to 2 and
	// populates only the first instance, then blocks until the reader has
	// correlated. Reading directly, the reader must observe {count == 2,
	// populated == 1}: a half-applied cross-store transaction, proving the
	// opt-in hole.
	var directViolation atomic.Int64
	{
		readNow := make(chan struct{})  // writer -> reader: count ahead of populated indices
		readDone := make(chan struct{}) // reader -> writer: correlation taken, finish the txn
		writeDone := make(chan struct{})

		go func() {
			defer close(writeDone)
			// Bump the counter a second time INSIDE the gap so the count (2)
			// leads the populated indices (1) at the moment the reader looks.
			_ = applyTwoParallelEdges(func() {
				g.IncEdgeCreateCount("a", "b") // count -> 2, second instance not yet populated
				close(readNow)
				<-readDone
				g.DecEdgeCreateCount("a", "b") // undo the manual bump; the real i2 re-bumps below
			})
		}()

		<-readNow
		if crossStoreViolated() {
			directViolation.Add(1)
		}
		close(readDone)
		<-writeDone
	}

	if directViolation.Load() == 0 {
		t.Fatalf("direct (no-View) cross-store read did not observe the documented partial-transaction hole; " +
			"expected violation > 0")
	}

	// The fully-applied state must be self-consistent: count == 2, both
	// instances populated. This also confirms the writer committed cleanly.
	if c := g.EdgeCreateCount("a", "b"); c != 2 {
		t.Fatalf("after commit EdgeCreateCount = %d, want 2", c)
	}
	if n := countPopulatedInstances(2); n != 2 {
		t.Fatalf("after commit populated instances = %d, want 2", n)
	}

	// Reset to a clean single-pair graph for half 2 so the count starts at 0.
	g2 := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g2.AddNode("a"); err != nil {
		t.Fatalf("g2 AddNode a: %v", err)
	}
	if err := g2.AddNode("b"); err != nil {
		t.Fatalf("g2 AddNode b: %v", err)
	}
	apply2 := func(beforeSecond func()) error {
		return g2.ApplyAtomically(func() error {
			h1, err := g2.AddEdgeH("a", "b", 0)
			if err != nil {
				return err
			}
			i1 := g2.IncEdgeCreateCount("a", "b")
			g2.SetEdgePropertyAt("a", "b", i1, "seq", Int64Value(i1))
			g2.SetEdgePropertyByHandle("a", "b", h1, "seq", Int64Value(i1))
			if beforeSecond != nil {
				beforeSecond()
			}
			h2, err := g2.AddEdgeH("a", "b", 0)
			if err != nil {
				return err
			}
			i2 := g2.IncEdgeCreateCount("a", "b")
			g2.SetEdgePropertyAt("a", "b", i2, "seq", Int64Value(i2))
			g2.SetEdgePropertyByHandle("a", "b", h2, "seq", Int64Value(i2))
			return nil
		})
	}
	viewCrossStoreViolated := func() bool {
		c := g2.EdgeCreateCount("a", "b")
		var n int64
		for idx := int64(1); idx <= c; idx++ {
			if len(g2.EdgePropertiesAt("a", "b", idx)) > 0 {
				n++
			}
		}
		return c != n
	}

	// Half 2 — the SAME correlation wrapped in View. The View read blocks until
	// the whole transaction is visible (the writer holds visMu for the entire
	// apply), so it can only observe count==0 (before) or count==2 with both
	// instances populated (after): zero violations. We synchronise the writer's
	// start with the reader so the View call genuinely contends with the apply.
	var viewViolation atomic.Int64
	var viewReads atomic.Int64
	{
		var wg sync.WaitGroup
		startWrite := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startWrite
			_ = apply2(func() {
				runtime.Gosched() // widen the partial window the View must mask
			})
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			close(startWrite)
			for i := 0; i < 2000; i++ {
				g2.View(func() {
					viewReads.Add(1)
					if viewCrossStoreViolated() {
						viewViolation.Add(1)
					}
				})
			}
		}()

		wg.Wait()
	}

	if v := viewViolation.Load(); v != 0 {
		t.Fatalf("View-wrapped cross-store read observed %d partial-transaction violations; View must close the window", v)
	}
	if viewReads.Load() == 0 {
		t.Fatal("View readers never read; half 2 did not exercise the invariant")
	}

	t.Logf("characterized cross-store opt-in barrier: direct reads observed %d violation(s); "+
		"View reads observed 0 across %d reads",
		directViolation.Load(), viewReads.Load())
}
