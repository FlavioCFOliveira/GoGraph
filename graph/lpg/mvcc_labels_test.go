package lpg

// mvcc_labels_test.go — correctness of the P0 spike's visibility semantics
// (rmp #2275).
//
// Layer: short.
//
// A cost measurement on a prototype that answers WRONGLY is worthless, so the
// visibility rule is pinned here even though the spike ships disabled. The rule
// is Memgraph's, and its three cases are what the test enumerates: a reader
// sees its own uncommitted change, never another transaction's uncommitted
// change, and a committed change only if it committed before the reader began.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestLabelDelta_VisibilityRule(t *testing.T) {
	const myTx = txIDBase + 7
	const otherTx = txIDBase + 9
	cases := []struct {
		name    string
		deltaTS uint64
		startTS uint64
		txID    uint64
		want    bool // must the delta be undone (i.e. is the change invisible)?
	}{
		{"my own uncommitted change is visible", myTx, 100, myTx, false},
		{"another transaction's uncommitted change is never visible", otherTx, 100, myTx, true},
		{"committed before I started: visible", 50, 100, myTx, false},
		{"committed exactly when I started: visible", 100, 100, myTx, false},
		{"committed after I started: not visible", 150, 100, myTx, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := &commitInfo{}
			info.ts.Store(c.deltaTS)
			d := &nodeLabelDelta{info: info}
			if got := d.mustUndo(c.startTS, c.txID); got != c.want {
				t.Fatalf("mustUndo(startTS=%d, txID=%d) with delta ts=%d = %v, want %v",
					c.startTS, c.txID, c.deltaTS, got, c.want)
			}
		})
	}
}

func TestLabelDelta_ReconstructsOlderVersion(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "Base"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	// Arm AFTER seeding, so "Base" is the committed state with no delta.
	g.EnableLabelDeltas()
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("node a not interned")
	}
	baseline := g.readTS() // a reader that started before the change below

	if err := g.SetNodeLabel("a", "Hot"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if n := g.LabelDeltaCount(); n != 1 {
		t.Fatalf("expected one delta after one real change, got %d", n)
	}

	// A reader that started BEFORE the change must not see "Hot".
	old := g.labelBagAsOf(id, baseline, 0)
	if !old.has(g.reg.Intern("Base")) {
		t.Fatal("the older version lost the label it had")
	}
	if old.has(g.reg.Intern("Hot")) {
		t.Fatal("a reader that started before the change can see it: the delta was not applied")
	}
	// A reader that started AFTER must see it.
	now := g.labelBagAsOf(id, g.readTS(), 0)
	if !now.has(g.reg.Intern("Hot")) {
		t.Fatal("a reader that started after the change cannot see it")
	}
	// The stored version must be untouched by the reconstruction.
	stored := g.labelBagPlain(id)
	if !stored.has(g.reg.Intern("Hot")) {
		t.Fatal("reconstructing an older version mutated the stored one")
	}
}

func TestLabelDelta_NoDeltaForARedundantWrite(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.EnableLabelDeltas()
	for i := 0; i < 10; i++ {
		if err := g.SetNodeLabel("a", "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	// MERGE's MATCH branch re-asserts labels on every match, so a delta per
	// STATEMENT rather than per real change would make the chain grow without
	// bound on an ordinary workload.
	if n := g.LabelDeltaCount(); n != 1 {
		t.Fatalf("ten identical SetNodeLabel calls produced %d deltas, want 1: a re-assertion "+
			"that changes nothing must not record a version that never existed", n)
	}
	g.RemoveNodeLabel("a", "L")
	g.RemoveNodeLabel("a", "L")
	if n := g.LabelDeltaCount(); n != 2 {
		t.Fatalf("a real removal plus a redundant one produced %d deltas, want 2", n)
	}
}

func TestLabelDelta_DisabledByDefault(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	g.RemoveNodeLabel("a", "L")
	if n := g.LabelDeltaCount(); n != 0 {
		t.Fatalf("the spike recorded %d deltas without being armed; it must ship inert", n)
	}
}
