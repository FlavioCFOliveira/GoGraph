package btree_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// stringProject projects a change payload that is already a plain string.
func stringProject(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// newTestBound builds a bound string index over a tiny in-memory model where
// every node is eligible and carries the value last set via Apply.
func newTestBound(t *testing.T, current map[graph.NodeID]string) *btree.Index[string] {
	t.Helper()
	idx, err := btree.NewBound(btree.Binding[string]{
		PropertyID:   7,
		LabelID:      3,
		Label:        "L",
		Property:     "p",
		Project:      stringProject,
		Eligible:     func(graph.NodeID) bool { return true },
		CurrentValue: func(n graph.NodeID) (string, bool) { v, ok := current[n]; return v, ok },
	})
	if err != nil {
		t.Fatalf("NewBound: %v", err)
	}
	return idx
}

func count(idx *btree.Index[string], lo, hi string) uint64 {
	return idx.Range(lo, hi).GetCardinality()
}

func TestBoundApply_InsertUpdateDelete(t *testing.T) {
	cur := map[graph.NodeID]string{}
	idx := newTestBound(t, cur)

	// Insert: SetNodeProperty with no old value.
	cur[1] = "banana"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Property: 7, Node: 1, NewValue: "banana"})
	if c := count(idx, "a", "z"); c != 1 {
		t.Fatalf("after insert: count=%d want 1", c)
	}

	// Update: old value must be removed, new inserted (the stale-key hazard).
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Property: 7, Node: 1, OldValue: "banana", NewValue: "cherry"})
	cur[1] = "cherry"
	if c := count(idx, "a", "bz"); c != 0 {
		t.Fatalf("after update: stale 'banana' key still present (count [a,bz]=%d, want 0)", c)
	}
	if c := count(idx, "c", "cz"); c != 1 {
		t.Fatalf("after update: 'cherry' missing (count [c,cz]=%d, want 1)", c)
	}

	// Property delete: node drops out entirely.
	idx.Apply(index.Change{Op: index.OpDelNodeProperty, Property: 7, Node: 1, OldValue: "cherry"})
	if c := count(idx, "a", "z"); c != 0 {
		t.Fatalf("after del-prop: count=%d want 0", c)
	}
}

func TestBoundApply_LabelAddRemove(t *testing.T) {
	cur := map[graph.NodeID]string{2: "delta"}
	idx := newTestBound(t, cur)

	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Label: 3, Node: 2})
	if c := count(idx, "a", "z"); c != 1 {
		t.Fatalf("after add-label: count=%d want 1", c)
	}
	idx.Apply(index.Change{Op: index.OpRemoveNodeLabel, Label: 3, Node: 2})
	if c := count(idx, "a", "z"); c != 0 {
		t.Fatalf("after remove-label: count=%d want 0", c)
	}
}

func TestBoundApply_IgnoresOtherKeys(t *testing.T) {
	idx := newTestBound(t, map[graph.NodeID]string{})
	// Wrong property and wrong label must be ignored.
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Property: 99, Node: 1, NewValue: "x"})
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Label: 99, Node: 1})
	if c := count(idx, "", "\xff"); c != 0 {
		t.Fatalf("unrelated changes leaked: count=%d want 0", c)
	}
}

func TestUnboundApply_IsNoOp(t *testing.T) {
	idx := btree.New[string]()
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Property: 7, Node: 1, NewValue: "x"})
	if _, _, ok := idx.BoundNode(); ok {
		t.Fatal("unbound index reports BoundNode ok=true")
	}
	if c := count(idx, "", "\xff"); c != 0 {
		t.Fatalf("unbound Apply populated the index: count=%d", c)
	}
}

func TestRangeCount_ExactAndEarlyExit(t *testing.T) {
	idx := btree.New[int64]()
	vals := make([]int64, 0, 100)
	nodes := make([]graph.NodeID, 0, 100)
	for i := 0; i < 100; i++ {
		vals = append(vals, int64(i))
		nodes = append(nodes, graph.NodeID(i+1))
	}
	if err := idx.BulkLoad(vals, nodes); err != nil {
		t.Fatal(err)
	}
	// Exact count within budget.
	if c, exact := idx.RangeCount(10, 19, 1000); !exact || c != 10 {
		t.Fatalf("RangeCount[10,19]=%d exact=%v, want 10/true", c, exact)
	}
	// Early-exit: range of 50 with budget 9 must report >budget and not exact.
	if c, exact := idx.RangeCount(0, 49, 9); exact || c != 10 {
		t.Fatalf("RangeCount early-exit: got %d/%v, want budget+1(10)/false", c, exact)
	}
	// hi < lo is an empty, exact range.
	if c, exact := idx.RangeCount(50, 10, 1000); !exact || c != 0 {
		t.Fatalf("RangeCount inverted: got %d/%v, want 0/true", c, exact)
	}
}
