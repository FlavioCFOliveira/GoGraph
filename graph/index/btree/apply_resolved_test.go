package btree_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// apply_resolved_test.go — rmp #2793 gate for the B+ tree index. It mirrors
// graph/index/hash/apply_resolved_test.go arm for arm, because a fix proven on
// one index kind says nothing about the other: both are registered by the same
// CREATE INDEX, both are caught up by the same build log, and both carried the
// defect.
//
// See the hash file's header for what the two entry points mean. The property
// asserted here is the same: [btree.Index.Apply] resolves from the binding at
// call time and is unchanged, [btree.Index.ApplyResolved] takes the recorded
// state and never consults the binding, and the two agree arm for arm.

const (
	resolvedPropID  uint32 = 3
	resolvedLabelID uint32 = 5
	otherResolvedID uint32 = 9
)

// resolvedFixture is a mutable stand-in for the graph state the engine's binding
// closures read, with the reads counted so a test can assert whether the binding
// was consulted at all.
type resolvedFixture struct {
	t            *testing.T
	value        string
	hasValue     bool
	eligible     bool
	currentCalls int
	eligCalls    int
	forbid       bool
}

func (f *resolvedFixture) bind(t *testing.T) *btree.Index[string] {
	t.Helper()
	idx, err := btree.NewBound(btree.Binding[string]{
		PropertyID: resolvedPropID,
		LabelID:    resolvedLabelID,
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

// TestBTreeApply_StillResolvesFromTheBindingAtCallTime pins the LIVE fan-out,
// which rmp #2793 left untouched.
func TestBTreeApply_StillResolvesFromTheBindingAtCallTime(t *testing.T) {
	t.Parallel()
	f := &resolvedFixture{value: "first", hasValue: true, eligible: true, t: t}
	idx := f.bind(t)

	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: resolvedLabelID})
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
	idx.Apply(index.Change{Op: index.OpAddNodeLabel, Node: 2, Label: resolvedLabelID})
	if got := idx.Cardinality("second"); got != 1 {
		t.Fatalf("after the graph changed to %q, a second label add indexed %d entries for "+
			"it, want 1 — Apply must re-read, not reuse an earlier answer", "second", got)
	}
}

// TestBTreeApplyResolved_UsesTheRecordedStateAndNeverTheBinding is the fix
// itself at the index level.
func TestBTreeApplyResolved_UsesTheRecordedStateAndNeverTheBinding(t *testing.T) {
	t.Parallel()
	f := &resolvedFixture{value: "uncommitted-ghost", hasValue: true, eligible: false, t: t}
	idx := f.bind(t)
	f.forbid = true

	idx.ApplyResolved(
		index.Change{Op: index.OpAddNodeLabel, Node: 1, Label: resolvedLabelID},
		"committed", true)

	if got := idx.Cardinality("committed"); got != 1 {
		t.Errorf("the replay indexed %d entries for the RECORDED value, want 1", got)
	}
	if got := idx.Cardinality("uncommitted-ghost"); got != 0 {
		t.Errorf("the replay indexed %d entries for the value the graph holds NOW, want 0 — "+
			"that value belongs to a later instant and may be an open transaction's "+
			"uncommitted write", got)
	}

	f.eligible = true
	idx.ApplyResolved(
		index.Change{Op: index.OpSetNodeProperty, Node: 2, Property: resolvedPropID, NewValue: "later"},
		nil, false)
	if got := idx.Cardinality("later"); got != 0 {
		t.Errorf("the replay indexed %d entries for a node recorded as INELIGIBLE, want 0", got)
	}
}

// TestBTreeApplyResolved_MatchesApplyForTheSameState is the anti-drift gate:
// every arm, driven through both entry points against the same state, must leave
// identical contents.
func TestBTreeApplyResolved_MatchesApplyForTheSameState(t *testing.T) {
	t.Parallel()
	changes := []index.Change{
		{Op: index.OpAddNodeLabel, Node: 1, Label: resolvedLabelID},
		{Op: index.OpRemoveNodeLabel, Node: 1, Label: resolvedLabelID},
		{Op: index.OpSetNodeProperty, Node: 1, Property: resolvedPropID, OldValue: "old", NewValue: "new"},
		{Op: index.OpDelNodeProperty, Node: 1, Property: resolvedPropID, OldValue: "old"},
		{Op: index.OpSetNodeProperty, Node: 1, Property: otherResolvedID, NewValue: "other"},
		{Op: index.OpAddNodeLabel, Node: 1, Label: otherResolvedID},
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
				live := &resolvedFixture{value: st.value, hasValue: st.hasValue, eligible: st.eligible, t: t}
				liveIdx := live.bind(t)
				rec := &resolvedFixture{value: st.value, hasValue: st.hasValue, eligible: st.eligible, t: t}
				recIdx := rec.bind(t)

				for _, idx := range []*btree.Index[string]{liveIdx, recIdx} {
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
