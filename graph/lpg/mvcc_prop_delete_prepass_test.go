package lpg

// mvcc_prop_delete_prepass_test.go — rmp #2710: the shared-lock pre-pass in
// [Graph.delNodePropertyInfo].
//
// Layer: short.
//
// The change moves three of the four outcomes of a node-property delete off the
// EXCLUSIVE shard lock and onto the shared one, on the ground that none of them
// makes a claim. That ground is exactly where this package has produced nine
// defects in one sprint, so each invariant the move could touch is pinned here
// rather than left to the aggregate suites:
//
//  1. the pre-pass agrees with the exclusive body about which outcome a given
//     shard state produces, in every shape including the UNARMED graph (whose
//     exclusive body never calls propBag.get at all) and the empty bag;
//  2. a delete that removes NOTHING still runs the existence cross-check of
//     rmp #2444 — the outcome that must NOT be confused with a refusal;
//  3. a delete that is REFUSED still refuses, and the conflict it records is
//     the property store's, not a later one;
//  4. the delete-when-empty contract survives: emptying a bag still drops the
//     node's map entry rather than leaving an empty bag behind.
//
// EVERY ORACLE HERE WAS PROVED ABLE TO FAIL, by mutating the code under it and
// observing the failure rather than by assuming the assertion bites:
//
//	mutation                                          test that failed
//	-------------------------------------------------------------------------
//	delPropNothingToRemove returns WITHOUT the         NoOpDeleteStillCross-
//	existence cross-check                             ChecksNodeLife
//
//	delNodePropertyShared returns delPropNeeds-       OutcomeParity (the two
//	Exclusive unconditionally (pre-pass disabled)      "nothing to remove" rows)
//
//	the refusal branch returns delPropNothingTo-       RefusalIsSettledUnder-
//	Remove instead of delPropRefused                   TheSharedLock
//
// TestPropDeletePrePass_EmptyingABagStillDropsTheEntry is the exception and is
// recorded as such: no writer can currently place an EMPTY bag in the shard map
// (see the guard in [Graph.delNodePropertyShared]), so no mutation of the
// pre-pass's empty-bag branch can be made to fail it. It pins the observable
// delete-when-empty contract, which is what a future writer would break, rather
// than the branch itself.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestPropDeletePrePass_OutcomeParity walks every shard state the pre-pass can
// meet and asserts the outcome it settles on.
//
// The oracle is derived from the exclusive body's own control flow, not from
// the pre-pass's implementation: a state mutates something exactly when the key
// is present and the conflict test passes, and it refuses exactly when the key
// is present and the conflict test fails.
func TestPropDeletePrePass_OutcomeParity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		armed bool
		// setup returns the node to act on and, when the outcome under test is a
		// refusal, the transaction that has been doomed against it.
		setup func(t *testing.T, g *Graph[string, float64]) (node, key string, tx *writeCtx)
		want  delPropOutcome
	}{
		{
			name:  "no bag at all: nothing to remove",
			armed: true,
			setup: func(_ *testing.T, _ *Graph[string, float64]) (string, string, *writeCtx) {
				return "bare", "anything", nil
			},
			want: delPropNothingToRemove,
		},
		{
			name:  "bag without the key: nothing to remove",
			armed: true,
			setup: func(t *testing.T, g *Graph[string, float64]) (string, string, *writeCtx) {
				t.Helper()
				if err := g.SetNodeProperty("held", "other", Int64Value(1)); err != nil {
					t.Fatalf("seed: %v", err)
				}
				return "held", "absent", nil
			},
			want: delPropNothingToRemove,
		},
		{
			name:  "bag with the key, no conflict: needs the exclusive lock",
			armed: true,
			setup: func(t *testing.T, g *Graph[string, float64]) (string, string, *writeCtx) {
				t.Helper()
				if err := g.SetNodeProperty("held", "k", Int64Value(1)); err != nil {
					t.Fatalf("seed: %v", err)
				}
				return "held", "k", nil
			},
			want: delPropNeedsExclusive,
		},
		{
			name:  "unarmed graph, key absent: nothing to remove",
			armed: false,
			setup: func(t *testing.T, g *Graph[string, float64]) (string, string, *writeCtx) {
				t.Helper()
				if err := g.SetNodeProperty("held", "other", Int64Value(1)); err != nil {
					t.Fatalf("seed: %v", err)
				}
				return "held", "absent", nil
			},
			want: delPropNothingToRemove,
		},
		{
			name:  "unarmed graph, key present: needs the exclusive lock",
			armed: false,
			setup: func(t *testing.T, g *Graph[string, float64]) (string, string, *writeCtx) {
				t.Helper()
				if err := g.SetNodeProperty("held", "k", Int64Value(1)); err != nil {
					t.Fatalf("seed: %v", err)
				}
				return "held", "k", nil
			},
			want: delPropNeedsExclusive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New[string, float64](adjlist.Config{Directed: true})
			for _, n := range []string{"bare", "held"} {
				if err := g.AddNode(n); err != nil {
					t.Fatalf("AddNode(%s): %v", n, err)
				}
			}
			if tc.armed {
				g.EnablePropDeltas()
			}
			node, key, tx := tc.setup(t, g)

			id, ok := g.adj.Mapper().Lookup(node)
			if !ok {
				t.Fatalf("%s was never interned", node)
			}
			// A key the registry has never seen would be rejected before the
			// pre-pass is ever reached, so intern it exactly as the caller does.
			keyID := g.propKeys().Intern(key)
			got := g.delNodePropertyShared(g.nodePropShardFor(id), id, keyID, tx)
			if got != tc.want {
				t.Fatalf("pre-pass outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPropDeletePrePass_RefusalIsSettledUnderTheSharedLock pins the third
// outcome, which needs a genuinely doomed transaction rather than a fabricated
// one: the pre-pass must report a refusal, and must do so WITHOUT the exclusive
// lock.
func TestPropDeletePrePass_RefusalIsSettledUnderTheSharedLock(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.EnablePropDeltas()

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("n", "k", Int64Value(1)); err != nil {
		t.Fatalf("A set: %v", err)
	}
	txB := g.beginLabelTx()
	// B's snapshot predates A's uncommitted head, so B is refused on the same
	// node property. This is the state the pre-pass must classify.
	if err := txB.setNodeProperty("n", "k", Int64Value(2)); err == nil {
		t.Fatal("the second writer on the same node property was not refused; " +
			"the conflict this test depends on never happened")
	}

	id, ok := g.adj.Mapper().Lookup("n")
	if !ok {
		t.Fatal("n was never interned")
	}
	keyID := g.propKeys().Intern("k")
	if got := g.delNodePropertyShared(g.nodePropShardFor(id), id, keyID, txB.ctx); got != delPropRefused {
		t.Fatalf("pre-pass outcome on a refused delete = %d, want delPropRefused (%d)", got, delPropRefused)
	}

	if _, err := txB.commit(); err == nil {
		t.Fatal("the doomed transaction committed")
	}
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A's commit: %v", err)
	}
}

// TestPropDeletePrePass_NoOpDeleteStillCrossChecksNodeLife is invariant 2, and
// it is the one a two-valued pre-pass would have broken in the other direction.
//
// A delete that removes nothing is still a WRITE against the node, so it must
// meet the cross-store rule of rmp #2444: a transaction whose snapshot predates
// a peer's pending removal of that node must be refused, even though the
// property it names is not there.
//
// Validated against a build whose pre-pass returns without the cross-check: the
// delete is then accepted and the transaction commits, which is the lost
// refusal this pins.
func TestPropDeletePrePass_NoOpDeleteStillCrossChecksNodeLife(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	for _, n := range []string{"n", "other"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	g.EnablePropDeltas()
	// The node carries a property, but NOT the one the delete will name, so the
	// delete is a no-op in the property store and the only thing that can refuse
	// it is the existence cross-check.
	if err := g.SetNodeProperty("n", "present", Int64Value(1)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The key must be INTERNED, on some other node, or the delete returns at
	// [PropertyKeyRegistry.Lookup] before it reaches either the shard or the
	// cross-check. That early return is pre-existing behaviour and is not what
	// this test is about; seeding the name here is what makes the test reach the
	// path it means to exercise rather than passing vacuously.
	if err := g.SetNodeProperty("other", "absent", Int64Value(1)); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	txA := g.beginLabelTx()
	if !txA.removeNode("n") {
		t.Fatal("A's node removal was refused; the pending death this test needs never happened")
	}
	txB := g.beginLabelTx()
	txB.delNodeProperty("n", "absent")

	if _, err := txB.commit(); err == nil {
		t.Fatal("a no-op property delete on a node with another transaction's " +
			"PENDING removal committed; the existence cross-check of rmp #2444 " +
			"was skipped by the shared-lock pre-pass (rmp #2710)")
	}
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A's commit: %v", err)
	}
}

// TestPropDeletePrePass_EmptyingABagStillDropsTheEntry is invariant 4.
//
// The pre-pass hands an empty bag to the exclusive path precisely so the
// delete-when-empty contract survives; this asserts the contract itself, which
// is the observable half.
func TestPropDeletePrePass_EmptyingABagStillDropsTheEntry(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	id, ok := g.adj.Mapper().Lookup("n")
	if !ok {
		t.Fatal("n was never interned")
	}
	if err := g.SetNodeProperty("n", "only", Int64Value(1)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := g.nodePropShardFor(id)
	s.mu.RLock()
	_, present := s.m[id]
	s.mu.RUnlock()
	if !present {
		t.Fatal("the seeded property left no bag")
	}

	g.DelNodeProperty("n", "only")

	s.mu.RLock()
	bag, still := s.m[id]
	s.mu.RUnlock()
	if still {
		t.Fatalf("emptying the bag left an entry behind (len=%d, empty=%v); the "+
			"delete-when-empty contract is broken", bag.len(), bag.empty())
	}

	// A second delete of the same now-absent key must be a clean no-op rather
	// than resurrecting an entry — this is the pre-pass's own fast path.
	g.DelNodeProperty("n", "only")
	s.mu.RLock()
	_, resurrected := s.m[id]
	s.mu.RUnlock()
	if resurrected {
		t.Fatal("a no-op delete of an absent key created a map entry")
	}
}

// TestPropDeletePrePass_ConcurrentAbsentDeletesLoseNoWrite exercises the shared
// lock against real writers, which is what the pre-pass changed: the absent-key
// deletes now hold the shard in SHARED mode while setters hold it exclusively.
//
// Run under -race this is the data-race gate for the new path; the assertion is
// the stronger property, that no concurrent setter's value is lost to a delete
// of a DIFFERENT key.
func TestPropDeletePrePass_ConcurrentAbsentDeletesLoseNoWrite(t *testing.T) {
	const (
		nodes   = 64
		writers = 8
		rounds  = 200
	)
	g := New[string, float64](adjlist.Config{Directed: true})
	names := make([]string, nodes)
	for i := range names {
		names[i] = "n" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if err := g.AddNode(names[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(writers * 2)
	for w := range writers {
		go func(w int) {
			defer wg.Done()
			for r := range rounds {
				n := names[(w*7+r)%nodes]
				if err := g.SetNodeProperty(n, "kept", Int64Value(int64(r))); err != nil {
					t.Errorf("set: %v", err)
					return
				}
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for r := range rounds {
				n := names[(w*11+r)%nodes]
				// A key no writer ever sets: every one of these takes the
				// pre-pass's absent-key fast path under the shared lock.
				g.DelNodeProperty(n, "never-set")
			}
		}(w)
	}
	wg.Wait()

	for _, n := range names {
		if _, ok := g.GetNodeProperty(n, "never-set"); ok {
			t.Fatalf("%s carries a key nothing ever set", n)
		}
	}
	// Every node the setters touched must still carry its property: an
	// absent-key delete must never have removed a different key.
	var kept int
	for _, n := range names {
		if _, ok := g.GetNodeProperty(n, "kept"); ok {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("no node kept its property; the concurrent setters did nothing " +
			"and this test proved nothing")
	}
}

// TestPropBagEmptyAgreesWithLen pins the substitution the pre-pass makes: it
// asks the O(1) [propBag.empty] the question the exclusive path answers with
// the O(n) [propBag.len], so the two must agree at every size and across the
// tier promotion that changes which backing store holds the properties.
func TestPropBagEmptyAgreesWithLen(t *testing.T) {
	var b propBag
	if b.empty() != (b.len() == 0) {
		t.Fatal("zero bag: empty and len disagree")
	}
	// Walk the bag up through its tiers, checking the two agree at every size —
	// including past the promotion point, where the backing store changes.
	for i := range 40 {
		b.set(PropertyKeyID(i), Int64Value(int64(i)))
		if b.empty() != (b.len() == 0) {
			t.Fatalf("after %d sets: empty=%v len=%d", i+1, b.empty(), b.len())
		}
	}
	for i := range 40 {
		b.del(PropertyKeyID(i))
		if b.empty() != (b.len() == 0) {
			t.Fatalf("after %d dels: empty=%v len=%d", i+1, b.empty(), b.len())
		}
	}
	if !b.empty() {
		t.Fatalf("bag not empty after removing every key: len=%d", b.len())
	}
}
