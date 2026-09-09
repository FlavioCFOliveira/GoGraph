package hash

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// apply_resolved_test.go — rmp #2793 gate for the hash index: [Index.Apply] and
// [Index.ApplyResolved] are the SAME rules over two different sources of the
// node state, and the live one is unchanged.
//
// The tests are structured around the one property that matters. Apply resolves
// the node's eligibility and current value FROM THE BINDING, at the moment it is
// called — that is the live fan-out, and it must stay exactly that. ApplyResolved
// takes both from a recording made when the change was fanned out and must not
// consult the binding at all, because the graph it would consult is a later one.
//
// The differential test is what keeps the two honest: it drives every arm
// through both entry points against the same state and requires byte-identical
// index contents, so a rule that is changed in one place and not the other fails
// here rather than in production.

// fakeBinding is a [Binding] over a mutable in-test "graph" whose reads are
// counted, so a test can assert not only the resulting content but WHETHER the
// binding was consulted at all.
type fakeBinding struct {
	value        string
	hasValue     bool
	eligible     bool
	currentCalls int
	eligCalls    int
	forbid       bool
	t            *testing.T
}

// bind builds an index bound to f.
func (f *fakeBinding) bind(t *testing.T) *Index[string] {
	t.Helper()
	idx, err := NewBound(Binding[string]{
		PropertyID: testPropID,
		LabelID:    testLabelID,
		Label:      "L",
		Property:   "p",
		Project: func(v any) (string, bool) {
			s, ok := v.(string)
			return s, ok
		},
		Eligible: func(graph.NodeID) bool {
			if f.forbid {
				f.t.Errorf("Eligible was consulted while replaying a recorded change: the " +
					"replay must use the state captured when the change was fanned out")
			}
			f.eligCalls++
			return f.eligible
		},
		CurrentValue: func(graph.NodeID) (string, bool) {
			if f.forbid {
				f.t.Errorf("CurrentValue was consulted while replaying a recorded change: the " +
					"replay must use the state captured when the change was fanned out")
			}
			f.currentCalls++
			return f.value, f.hasValue
		},
	})
	if err != nil {
		t.Fatalf("NewBound: %v", err)
	}
	return idx
}

// TestApply_StillResolvesFromTheBindingAtCallTime pins the LIVE fan-out, which
// rmp #2793 left untouched: Apply reads the node state through the binding,
// every time it is called, so the index converges on the state the graph is in
// at that moment.
//
// It is asserted by making the binding's answer change between two calls: an
// index that had cached, recorded, or otherwise frozen the first answer would
// insert the first value twice.
func TestApply_StillResolvesFromTheBindingAtCallTime(t *testing.T) {
	t.Parallel()
	f := &fakeBinding{value: "first", hasValue: true, eligible: true, t: t}
	idx := f.bind(t)

	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: testLabelID})
	if f.currentCalls == 0 || f.eligCalls == 0 {
		t.Fatalf("Apply consulted the binding %d times for the current value and %d for "+
			"eligibility, want both non-zero — the live fan-out resolves from the graph",
			f.currentCalls, f.eligCalls)
	}
	if got := idx.Cardinality("first"); got != 1 {
		t.Fatalf("after a label add with the graph holding %q, the index holds %d entries "+
			"for it, want 1", "first", got)
	}

	f.value = "second"
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 2, Label: testLabelID})
	if got := idx.Cardinality("second"); got != 1 {
		t.Fatalf("after the graph changed to %q, a second label add indexed %d entries for "+
			"it, want 1 — Apply must re-read, not reuse an earlier answer", "second", got)
	}
}

// TestApplyResolved_UsesTheRecordedStateAndNeverTheBinding is the fix itself at
// the index level: the replay applies what the recording says, and does not ask
// the graph, whose answer at replay time belongs to a later instant.
func TestApplyResolved_UsesTheRecordedStateAndNeverTheBinding(t *testing.T) {
	t.Parallel()
	// The "graph" says one thing…
	f := &fakeBinding{value: "uncommitted-ghost", hasValue: true, eligible: false, t: t}
	idx := f.bind(t)
	// …and consulting it at all is a failure.
	f.forbid = true

	idx.ApplyResolved(
		index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: testLabelID},
		"committed", true)

	if got := idx.Cardinality("committed"); got != 1 {
		t.Errorf("the replay indexed %d entries for the RECORDED value, want 1", got)
	}
	if got := idx.Cardinality("uncommitted-ghost"); got != 0 {
		t.Errorf("the replay indexed %d entries for the value the graph holds NOW, want 0 — "+
			"that value belongs to a later instant and may be an open transaction's "+
			"uncommitted write", got)
	}

	// The eligibility door, the other way round: a recorded change whose node was
	// NOT eligible must not be inserted, whatever the graph says now.
	f.eligible = true
	idx.ApplyResolved(
		index.Change{Op: index.OpSetNodeProperty, Node: 2, Property: testPropID, NewValue: "later"},
		nil, false)
	if got := idx.Cardinality("later"); got != 0 {
		t.Errorf("the replay indexed %d entries for a node recorded as INELIGIBLE, want 0", got)
	}
}

// TestApplyResolved_MatchesApplyForTheSameState is the anti-drift gate. Every
// arm is driven through both entry points against the same node state, and the
// two must produce identical contents — so a rule changed on one path and not
// the other fails here.
//
// The states cover the combinations the arms actually branch on: value present
// or absent, eligible or not.
func TestApplyResolved_MatchesApplyForTheSameState(t *testing.T) {
	t.Parallel()
	changes := []index.Change{
		{Op: index.OpAddNodeLabel, Node: 1, Label: testLabelID},
		{Op: index.OpRemoveNodeLabel, Node: 1, Label: testLabelID},
		{Op: index.OpSetNodeProperty, Node: 1, Property: testPropID, OldValue: "old", NewValue: "new"},
		{Op: index.OpDelNodeProperty, Node: 1, Property: testPropID, OldValue: "old"},
		// Changes the arms must ignore: a different property, a different label.
		{Op: index.OpSetNodeProperty, Node: 1, Property: otherPropID, NewValue: "other"},
		{Op: index.OpAddNodeLabel, Node: 1, Label: otherLabel},
	}
	states := []struct {
		name     string
		value    string
		hasValue bool
		eligible bool
	}{
		{"present/eligible", "cur", true, true},
		{"present/ineligible", "cur", true, false},
		{"absent/eligible", "", false, true},
		{"absent/ineligible", "", false, false},
	}
	probes := []string{"cur", "old", "new", "other", ""}

	for _, st := range states {
		for k, c := range changes {
			t.Run(st.name, func(t *testing.T) {
				live := &fakeBinding{value: st.value, hasValue: st.hasValue, eligible: st.eligible, t: t}
				liveIdx := live.bind(t)
				rec := &fakeBinding{value: st.value, hasValue: st.hasValue, eligible: st.eligible, t: t}
				recIdx := rec.bind(t)

				// Both start from the same non-empty content, so a DELETE arm has
				// something to remove and a difference is observable in both
				// directions.
				for _, idx := range []*Index[string]{liveIdx, recIdx} {
					idx.Insert("cur", 1)
					idx.Insert("old", 1)
				}

				liveIdx.Apply(c)
				var current any
				if st.hasValue {
					current = st.value
				}
				rec.forbid = true
				recIdx.ApplyResolved(c, current, st.eligible)

				for _, p := range probes {
					if a, b := liveIdx.Cardinality(p), recIdx.Cardinality(p); a != b {
						t.Fatalf("change[%d] (%+v) under state %s: Apply left %d entries for %q "+
							"and ApplyResolved left %d — the live rules and the replay rules have "+
							"drifted apart", k, c, st.name, a, p, b)
					}
				}
			})
		}
	}
}
