package hash

// bound_test.go — task #1340: a bound hash index (NewBound) maintains itself
// from the index.Manager change fan-out, while the unbound index (New) keeps
// its historical no-op Apply.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

const (
	testPropID  = 0 // interned IDs start at zero — the binding must not treat 0 as "absent"
	testLabelID = 0
	otherPropID = 7
	otherLabel  = 9
)

// boundFixture is a minimal stand-in for the graph state the engine's binding
// closures read: per-node liveness, label membership, and the current value of
// the bound property. Tests mutate it to model the FINAL state the closures
// observe at commit time.
type boundFixture struct {
	labelled map[graph.NodeID]bool
	value    map[graph.NodeID]string
	dead     map[graph.NodeID]bool
}

func newBoundFixture() *boundFixture {
	return &boundFixture{
		labelled: make(map[graph.NodeID]bool),
		value:    make(map[graph.NodeID]string),
		dead:     make(map[graph.NodeID]bool),
	}
}

func (f *boundFixture) binding() Binding[string] {
	return Binding[string]{
		PropertyID: testPropID,
		LabelID:    testLabelID,
		Label:      "Person",
		Property:   "name",
		Project: func(v any) (string, bool) {
			s, ok := v.(string)
			return s, ok
		},
		Eligible: func(id graph.NodeID) bool {
			return !f.dead[id] && f.labelled[id]
		},
		CurrentValue: func(id graph.NodeID) (string, bool) {
			if f.dead[id] {
				return "", false
			}
			s, ok := f.value[id]
			return s, ok
		},
	}
}

func newBoundIndex(t *testing.T, f *boundFixture) *Index[string] {
	t.Helper()
	idx, err := NewBound(f.binding())
	if err != nil {
		t.Fatalf("NewBound: %v", err)
	}
	return idx
}

func TestNewBound_RejectsIncompleteBinding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Binding[string])
	}{
		{"missing label", func(b *Binding[string]) { b.Label = "" }},
		{"missing property", func(b *Binding[string]) { b.Property = "" }},
		{"missing project", func(b *Binding[string]) { b.Project = nil }},
		{"missing eligible", func(b *Binding[string]) { b.Eligible = nil }},
		{"missing current value", func(b *Binding[string]) { b.CurrentValue = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoundFixture().binding()
			tc.mutate(&b)
			if _, err := NewBound(b); err == nil {
				t.Fatal("NewBound: want error for incomplete binding, got nil")
			}
		})
	}
}

func TestUnboundApply_RemainsNoOp(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	idx.Apply(index.Change{
		Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "x",
	})
	if idx.Cardinality("x") != 0 {
		t.Fatal("unbound Apply must remain a no-op")
	}
}

func TestBoundApply_SetInsertsWhenEligible(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)

	// Final state: node 1 labelled with name "alice".
	f.labelled[1] = true
	f.value[1] = "alice"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})

	if !idx.Contains("alice", 1) {
		t.Fatal("eligible node must be indexed on SetNodeProperty")
	}
}

func TestBoundApply_SetSkipsUnlabelledNode(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)

	f.value[1] = "alice" // node 1 never gets the label
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("node without the bound label must not be indexed")
	}
}

func TestBoundApply_SetMovesOldValue(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true

	f.value[1] = "alice"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})
	f.value[1] = "alicia"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, OldValue: "alice", NewValue: "alicia"})

	if idx.Contains("alice", 1) {
		t.Fatal("stale entry for the old value must be deleted")
	}
	if !idx.Contains("alicia", 1) {
		t.Fatal("new value must be indexed")
	}
}

func TestBoundApply_DelRemovesEntry(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true
	f.value[1] = "alice"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})

	delete(f.value, 1)
	idx.Apply(index.Change{Op: index.OpDelNodeProperty, Node: 1, Property: testPropID, OldValue: "alice"})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("DelNodeProperty must remove the entry")
	}
}

func TestBoundApply_IgnoresOtherPropertyAndLabel(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true
	f.value[1] = "alice"

	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: otherPropID, NewValue: "alice"})
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: otherLabel})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("changes for other properties/labels must be ignored")
	}
}

func TestBoundApply_LabelAddIndexesCurrentValue(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)

	// Node already carried the property while unlabelled; the label arrives now.
	f.value[1] = "alice"
	f.labelled[1] = true
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: testLabelID})

	if !idx.Contains("alice", 1) {
		t.Fatal("label add must index the node's current value")
	}
}

func TestBoundApply_LabelRemoveDropsCurrentValue(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true
	f.value[1] = "alice"
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: testLabelID})

	f.labelled[1] = false
	idx.Apply(index.Change{Op: index.OpRemoveNodeLabel, Node: 1, Label: testLabelID})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("label remove must drop the node from the index")
	}
}

// TestBoundApply_InterleavedBatchOrders replays the two orderings of a batch
// that both removes the label and changes the property, asserting the index
// converges to the same (empty) final state regardless of replay order. This
// pins the design rule that property changes delete their old value
// unconditionally while label events read the final-state current value.
func TestBoundApply_InterleavedBatchOrders(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, order []index.Change) {
		f := newBoundFixture()
		idx := newBoundIndex(t, f)
		// Pre-state: labelled node indexed under "alice".
		f.labelled[1] = true
		f.value[1] = "alice"
		idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})

		// Final state after the batch: unlabelled, value "alicia".
		f.labelled[1] = false
		f.value[1] = "alicia"
		for _, c := range order {
			idx.Apply(c)
		}
		if got := idx.Cardinality("alice") + idx.Cardinality("alicia"); got != 0 {
			t.Fatalf("index must be empty after label removal, found %d entries", got)
		}
	}
	setProp := index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, OldValue: "alice", NewValue: "alicia"}
	rmLabel := index.Change{Op: index.OpRemoveNodeLabel, Node: 1, Label: testLabelID}

	t.Run("set then remove-label", func(t *testing.T) {
		t.Parallel()
		run(t, []index.Change{setProp, rmLabel})
	})
	t.Run("remove-label then set", func(t *testing.T) {
		t.Parallel()
		run(t, []index.Change{rmLabel, setProp})
	})
}

func TestBoundApply_NodeDeletionViaPropertyRemoval(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true
	f.value[1] = "alice"
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, NewValue: "alice"})

	// The engine enqueues per-property deletions then per-label removals when
	// a node is removed; the node is dead by the time the batch applies.
	f.dead[1] = true
	idx.Apply(index.Change{Op: index.OpDelNodeProperty, Node: 1, Property: testPropID, OldValue: "alice"})
	idx.Apply(index.Change{Op: index.OpRemoveNodeLabel, Node: 1, Label: testLabelID})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("deleted node must be fully unindexed")
	}
}

func TestBoundApply_IgnoresEdgeChanges(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	f.labelled[1] = true
	f.value[1] = "alice"

	idx.Apply(index.Change{Op: index.OpSetEdgeProperty, Node: 1, Dst: 2, Property: testPropID, NewValue: "alice"})
	idx.Apply(index.Change{Op: index.OpAddEdgeLabel, Node: 1, Dst: 2, Label: testLabelID})

	if idx.Cardinality("alice") != 0 {
		t.Fatal("edge changes must be ignored by a node-bound index")
	}
}

func TestBoundNode_Accessor(t *testing.T) {
	t.Parallel()
	f := newBoundFixture()
	idx := newBoundIndex(t, f)
	label, prop, ok := idx.BoundNode()
	if !ok || label != "Person" || prop != "name" {
		t.Fatalf("BoundNode = (%q, %q, %v), want (Person, name, true)", label, prop, ok)
	}
	_, _, ok = New[string]().BoundNode()
	if ok {
		t.Fatal("unbound index must report ok=false")
	}
}
