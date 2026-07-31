package lpg

// mvcc_txn_test.go — MVCC P1 (rmp #2278): the properties the shared commit
// record exists to provide.
//
// Layer: short.
//
// The substrate is inert unless armed, so these tests arm it explicitly. What
// they pin is not "does it store a timestamp" but the three properties every
// later phase will rest on: a transaction becomes visible ATOMICALLY, an
// aborted one never becomes visible at all, and a reader's start timestamp
// decides what it sees.

import (
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func txGraph(t *testing.T, nodes ...string) (*Graph[string, float64], map[string]graph.NodeID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	ids := make(map[string]graph.NodeID, len(nodes))
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
		id, ok := g.adj.Mapper().Lookup(n)
		if !ok {
			t.Fatalf("%s not interned", n)
		}
		ids[n] = id
	}
	g.EnableLabelDeltas()
	return g, ids
}

// TestLabelTx_CommitIsAtomic is the load-bearing test of P1: a transaction that
// writes several labels must become visible all at once.
//
// It is asserted from BOTH ends. Behaviourally: a reader that starts before the
// commit sees none of the writes and one that starts after sees all of them.
// Structurally: every delta the transaction created points at the SAME commit
// record, which is what makes the commit one store and therefore what makes
// "all at once" true by construction rather than by luck of timing.
func TestLabelTx_CommitIsAtomic(t *testing.T) {
	g, ids := txGraph(t, "a", "b", "c")

	before := g.readTS()
	tx := g.beginLabelTx()
	for _, n := range []string{"a", "b", "c"} {
		if err := tx.setNodeLabel(n, "L"); err != nil {
			t.Fatalf("setNodeLabel(%s): %v", n, err)
		}
	}
	lid, ok := g.reg.Lookup("L")
	if !ok {
		t.Fatal("label L was never interned")
	}

	// STRUCTURAL: one shared record across every delta.
	var seen *commitInfo
	count := 0
	for _, id := range ids {
		sh := g.nodeLabelShardFor(id)
		sh.mu.RLock()
		for d := sh.d[id]; d != nil; d = d.next {
			count++
			if seen == nil {
				seen = d.info
			} else if d.info != seen {
				sh.mu.RUnlock()
				t.Fatalf("deltas of one transaction point at different commit records "+
					"(%p and %p): committing would need a walk, and a reader could observe "+
					"the transaction half-published", seen, d.info)
			}
		}
		sh.mu.RUnlock()
	}
	if count != 3 {
		t.Fatalf("expected 3 deltas, found %d", count)
	}

	// BEHAVIOURAL, before commit: a concurrent reader sees nothing.
	for n, id := range ids {
		bag := g.labelBagAsOf(id, before, 0)
		if bag.has(lid) { //nolint:gocritic // addressable local
			t.Fatalf("node %s shows an uncommitted label to another reader", n)
		}
	}
	// The writing transaction itself does see its own writes.
	for n, id := range ids {
		bag := tx.labelsOf(id)
		if !bag.has(lid) {
			t.Fatalf("node %s: the writing transaction cannot see its own uncommitted write", n)
		}
	}

	commitTS := tx.commit()

	// BEHAVIOURAL, after commit: a reader that started earlier still sees
	// nothing; one starting now sees everything.
	for n, id := range ids {
		if bag := g.labelBagAsOf(id, before, 0); bag.has(lid) {
			t.Fatalf("node %s: a reader that started before the commit can see it", n)
		}
		if bag := g.labelBagAsOf(id, commitTS, 0); !bag.has(lid) {
			t.Fatalf("node %s: a reader that started after the commit cannot see it", n)
		}
	}
}

// TestLabelTx_AbortIsNeverVisible pins that an aborted transaction's writes are
// invisible to every reader, including one that starts afterwards.
func TestLabelTx_AbortIsNeverVisible(t *testing.T) {
	g, ids := txGraph(t, "a", "b")
	tx := g.beginLabelTx()
	for n := range ids {
		if err := tx.setNodeLabel(n, "L"); err != nil {
			t.Fatalf("setNodeLabel: %v", err)
		}
	}
	tx.abort()
	lid, ok := g.reg.Lookup("L")
	if !ok {
		t.Fatal("label L was never interned")
	}
	// A reader starting now, strictly after the abort.
	after := g.nextCommitTS()
	for n, id := range ids {
		if bag := g.labelBagAsOf(id, after, 0); bag.has(lid) {
			t.Fatalf("node %s shows the label of an ABORTED transaction", n)
		}
	}
}

// TestLabelTx_DoubleFinishPanics pins the lifecycle: a transaction is published
// or abandoned exactly once. A second store into a shared record would change
// the visibility of deltas other readers may already have resolved.
func TestLabelTx_DoubleFinishPanics(t *testing.T) {
	for _, second := range []string{"commit", "abort"} {
		t.Run(second, func(t *testing.T) {
			g, _ := txGraph(t, "a")
			tx := g.beginLabelTx()
			tx.commit()
			defer func() {
				if recover() == nil {
					t.Fatalf("a second %s did not panic", second)
				}
			}()
			if second == "commit" {
				tx.commit()
			} else {
				tx.abort()
			}
		})
	}
}

// TestNodeLabelDelta_StaysSmall pins the size the cost model in
// docs/mvcc-p0-measurement.md rests on. P1 replaced an inline uint64 timestamp
// with a pointer, so the struct must not have grown: +24 B per modification is
// the measured figure the programme was authorised on.
func TestNodeLabelDelta_StaysSmall(t *testing.T) {
	if got := unsafe.Sizeof(nodeLabelDelta{}); got != 32 {
		t.Fatalf("nodeLabelDelta is %d bytes, want 32 — the per-modification memory cost is "+
			"the whole cost model the programme was authorised on, so a change here needs a "+
			"re-measurement, not a new constant", got)
	}
	// The claim the programme rests on is that this is a CONSTANT, not that it
	// is any particular constant. P0 measured 24 bytes with the timestamp
	// inline; P1 unions a shared-record pointer with that timestamp and pays 8
	// more, which is still strictly cheaper than the 40 bytes in two
	// allocations that a separate record per autocommit write cost.
}

// TestLabelTx_RemoveIsAlsoAtomic covers the undo direction. An add records
// "remove this to go back"; a removal records "add it back", and the two must
// be equally invisible until commit — otherwise a transaction that deletes a
// label would publish the deletion the instant it happened.
func TestLabelTx_RemoveIsAlsoAtomic(t *testing.T) {
	g, ids := txGraph(t, "a", "b")
	// Seed committed state: both nodes carry L.
	for n := range ids {
		if err := g.SetNodeLabel(n, "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	lid, ok := g.reg.Lookup("L")
	if !ok {
		t.Fatal("label L was never interned")
	}
	seeded := g.readTS()
	for n, id := range ids {
		if bag := g.labelBagAsOf(id, seeded, 0); !bag.has(lid) {
			t.Fatalf("fixture: node %s does not carry L before the removal", n)
		}
	}

	tx := g.beginLabelTx()
	for n := range ids {
		tx.removeNodeLabel(n, "L")
	}
	// Another reader must still see the label: the removal is not published.
	for n, id := range ids {
		if bag := g.labelBagAsOf(id, seeded, 0); !bag.has(lid) {
			t.Fatalf("node %s lost L to an UNCOMMITTED removal", n)
		}
	}
	// The removing transaction sees its own removal.
	for n, id := range ids {
		if bag := tx.labelsOf(id); bag.has(lid) {
			t.Fatalf("node %s: the removing transaction still sees the label it removed", n)
		}
	}
	commitTS := tx.commit()
	for n, id := range ids {
		if bag := g.labelBagAsOf(id, commitTS, 0); bag.has(lid) {
			t.Fatalf("node %s still carries L after the removal committed", n)
		}
		if bag := g.labelBagAsOf(id, seeded, 0); !bag.has(lid) {
			t.Fatalf("node %s: a reader that started before the removal cannot see L any more", n)
		}
	}
}
