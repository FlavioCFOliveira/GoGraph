package lpg

import (
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
// This is a CONTRACT/characterization test, not a bug fix: it locks the behaviour
// described on those accessors and in docs/isolation-design.md, under a
// deterministic channel handshake (no flaky timing). What it still proves is that
// the direct correlation observes violation > 0.
//
// The reader requests only the stores' own shard locks, never the visibility gate,
// so the handshake cannot deadlock against the writer that holds it via
// [Graph.ApplyAtomically]. Run under -race: the per-shard locks make every access
// data-race-free; the gap proven OPEN here is logical cross-store
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
			if err := g.SetEdgePropertyAt("a", "b", i1, "seq", Int64Value(i1)); err != nil {
				return err
			}
			g.SetEdgeLabelByHandle("a", "b", h1, "R")
			if err := g.SetEdgePropertyByHandle("a", "b", h1, "seq", Int64Value(i1)); err != nil {
				return err
			}

			if beforeSecond != nil {
				beforeSecond()
			}

			h2, err := g.AddEdgeH("a", "b", 0)
			if err != nil {
				return err
			}
			i2 := g.IncEdgeCreateCount("a", "b")
			g.SetEdgeLabelAt("a", "b", i2, "R")
			if err := g.SetEdgePropertyAt("a", "b", i2, "seq", Int64Value(i2)); err != nil {
				return err
			}
			g.SetEdgeLabelByHandle("a", "b", h2, "R")
			if err := g.SetEdgePropertyByHandle("a", "b", h2, "seq", Int64Value(i2)); err != nil {
				return err
			}
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

	// HALF 2 IS RETRACTED, NOT MIGRATED (rmp #2351).
	//
	// It used to run the same correlation wrapped in [Graph.View] and assert ZERO
	// violations, because the writer held the barrier exclusively for the whole apply.
	// rmp #2344 removed Graph.View, and every other reader in that task moved to a
	// pinned SNAPSHOT — this one CANNOT. The two stores have different versioning
	// status: [Graph.EdgePropertiesAt] has an as-of form
	// ([Graph.EdgePropertiesAtAsOf], reachable through [ReadView]), and
	// [Graph.EdgeCreateCount] has NONE — it is a raw counter with no version chain.
	//
	// So the count side cannot be pinned to any instant, and the correlation asserted
	// here is not obtainable by ANY mechanism the module offers today. Asserting it
	// anyway would be asserting something the code cannot deliver; asserting it
	// through a half-pinned read would be worse, because it would pass by luck.
	//
	// rmp #2351 DECIDED it: one production reader does correlate them
	// (cypher/api.go edgeInstanceIdxFor), and it uses the pair as a CONSERVATIVE
	// GUARD, so a stale-high count makes the guard decline and the caller falls back
	// to the per-pair union of edge types — a loss of PRECISION, not a wrong row.
	// The counter is therefore documented as an ALLOCATION SEQUENCE that belongs to
	// no snapshot rather than versioned; see [Graph.EdgeCreateCount]. The hole
	// characterised above is consequently unconditional BY DESIGN, which is worth
	// stating out loud rather than leaving as a deleted test.
	t.Logf("characterised the cross-store hole: direct reads observed %d violation(s). "+
		"The View-wrapped half is RETRACTED — EdgeCreateCount is unversioned, so the "+
		"correlation is not obtainable at any instant (rmp #2351)",
		directViolation.Load())
}
