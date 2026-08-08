package exec

// label_constraints_zerocost_test.go — the zero-constraint label write must stay
// free (rmp #2352).
//
// Adding UNIQUE enforcement to the label path (cypher/exec/label_constraints.go)
// puts new work on a write that previously did none. The requirement is that with
// NO UNIQUE constraint registered the label write takes NO registry lock and
// makes NO new allocation. That is not a nicety:
// [ConstraintRegistry.uniqueActive] records that this one registry mutex was 57 %
// of ALL lock delay at sixteen concurrent writers on a schema with no constraints
// at all, once rmp #2306 let writers overlap.
//
// # How the lock claim is proved rather than asserted
//
// The test HOLDS the registry's own mutex exclusively and then drives the
// operator. If the operator reaches for that mutex it blocks for ever; if it
// finishes, it provably never touched it. That is a decisive answer, not a
// statistical one — no mutex profile, no sampling, no flakiness.
//
// Each such test is paired with a NEGATIVE CONTROL that registers a UNIQUE
// constraint and shows the very same probe DOES block. Without the control the
// test could pass because the probe cannot see anything at all.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// minimal harness
// ─────────────────────────────────────────────────────────────────────────────

// labelProbeMutator implements only the handful of [GraphMutator] methods the
// label operators call. The interface is EMBEDDED and left nil deliberately: any
// method these operators are not expected to use panics instead of silently
// answering, so an unnoticed new dependency fails loudly.
type labelProbeMutator struct {
	GraphMutator
	labels map[string]bool
	props  map[string]lpg.PropertyValue
	key    string
}

func newLabelProbeMutator(key string) *labelProbeMutator {
	return &labelProbeMutator{
		key:    key,
		labels: map[string]bool{},
		props:  map[string]lpg.PropertyValue{},
	}
}

func (m *labelProbeMutator) ResolveNodeLabel(graph.NodeID) (string, bool) { return m.key, true }
func (m *labelProbeMutator) SetNodeLabel(_, label string) error           { m.labels[label] = true; return nil }
func (m *labelProbeMutator) RemoveNodeLabel(_, label string)              { delete(m.labels, label) }

func (m *labelProbeMutator) NodeLabels(string) []string {
	out := make([]string, 0, len(m.labels))
	for l := range m.labels {
		out = append(out, l)
	}
	return out
}

func (m *labelProbeMutator) NodeProperties(string) map[string]lpg.PropertyValue { return m.props }

// oneRowSource yields a single row carrying one NodeID column, then EOF. It is
// re-armed by Init so one operator tree can be driven repeatedly.
type oneRowSource struct{ done bool }

func (s *oneRowSource) Init(context.Context) error { s.done = false; return nil }
func (s *oneRowSource) Close() error               { return nil }

func (s *oneRowSource) Next(out *Row) (bool, error) {
	if s.done {
		return false, nil
	}
	s.done = true
	*out = Row{expr.IntegerValue(1)}
	return true, nil
}

// driveOnce runs op through one full Init → Next → Next(EOF) cycle.
func driveOnce(op Operator) error {
	if err := op.Init(context.Background()); err != nil {
		return err
	}
	var row Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
}

// newSetLabelsProbe builds a SetLabels over a one-row source with reg attached.
func newSetLabelsProbe(reg *ConstraintRegistry) (Operator, *labelProbeMutator) {
	mut := newLabelProbeMutator("n1")
	op := NewSetLabels("n", []string{"Person"}, map[string]int{"n": 0}, &oneRowSource{}, mut)
	if reg != nil {
		op.WithConstraints(reg, nil)
	}
	return op, mut
}

// newRemoveLabelsProbe builds a RemoveLabels over a one-row source with reg
// attached. The node starts carrying the label so the operator does real work.
func newRemoveLabelsProbe(reg *ConstraintRegistry) (Operator, *labelProbeMutator) {
	mut := newLabelProbeMutator("n1")
	mut.labels["Person"] = true
	op := NewRemoveLabels("n", []string{"Person"}, map[string]int{"n": 0}, &oneRowSource{}, mut)
	if reg != nil {
		op.WithConstraintRegistry(reg)
	}
	return op, mut
}

// runsWithRegistryLockHeld drives op while the registry's own mutex is held
// EXCLUSIVELY, and reports whether it completed. False means the operator
// reached for that mutex and blocked.
//
// The lock is always released, and the worker always joined, so a blocked probe
// leaves no goroutine behind for goleak to find.
func runsWithRegistryLockHeld(t *testing.T, reg *ConstraintRegistry, op Operator) bool {
	t.Helper()
	done := make(chan error, 1)
	reg.mu.Lock()
	go func() { done <- driveOnce(op) }()

	var completed bool
	select {
	case err := <-done:
		completed = true
		reg.mu.Unlock()
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		reg.mu.Unlock()
		if err := <-done; err != nil { // joined so nothing leaks
			t.Fatalf("drive (after unblocking): %v", err)
		}
	}
	return completed
}

// ─────────────────────────────────────────────────────────────────────────────
// the lock claim
// ─────────────────────────────────────────────────────────────────────────────

// TestSetLabels_NoUniqueConstraint_TakesNoRegistryLock proves the zero-constraint
// SET n:Label never acquires the constraint registry's mutex.
func TestSetLabels_NoUniqueConstraint_TakesNoRegistryLock(t *testing.T) {
	reg := NewConstraintRegistry() // no constraint of any kind
	op, mut := newSetLabelsProbe(reg)
	if !runsWithRegistryLockHeld(t, reg, op) {
		t.Fatal("SET n:Label blocked on the constraint registry's lock with NO UNIQUE constraint registered")
	}
	if !mut.labels["Person"] { // the probe must have done its actual work
		t.Fatal("the operator completed without writing the label")
	}
}

// TestSetLabels_WithUniqueConstraint_DoesTakeRegistryLock is the negative
// control. It shows the probe above can detect a lock acquisition at all — the
// SAME probe blocks once a UNIQUE constraint makes the enforcement path live.
func TestSetLabels_WithUniqueConstraint_DoesTakeRegistryLock(t *testing.T) {
	reg := NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")
	op, _ := newSetLabelsProbe(reg)
	if runsWithRegistryLockHeld(t, reg, op) {
		t.Fatal("PROBE BLIND: SET n:Label completed while the registry lock was held, " +
			"so the zero-constraint test proves nothing")
	}
}

// TestRemoveLabels_NoUniqueConstraint_TakesNoRegistryLock is the same claim for
// the release direction.
func TestRemoveLabels_NoUniqueConstraint_TakesNoRegistryLock(t *testing.T) {
	reg := NewConstraintRegistry()
	op, mut := newRemoveLabelsProbe(reg)
	if !runsWithRegistryLockHeld(t, reg, op) {
		t.Fatal("REMOVE n:Label blocked on the constraint registry's lock with NO UNIQUE constraint registered")
	}
	if mut.labels["Person"] {
		t.Fatal("the operator completed without removing the label")
	}
}

// TestRemoveLabels_WithUniqueConstraint_DoesTakeRegistryLock is its control.
func TestRemoveLabels_WithUniqueConstraint_DoesTakeRegistryLock(t *testing.T) {
	reg := NewConstraintRegistry()
	reg.RegisterUnique("Person", "email", "__uniq__Person.email")
	op, _ := newRemoveLabelsProbe(reg)
	if runsWithRegistryLockHeld(t, reg, op) {
		t.Fatal("PROBE BLIND: REMOVE n:Label completed while the registry lock was held, " +
			"so the zero-constraint test proves nothing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the allocation claim
// ─────────────────────────────────────────────────────────────────────────────

// zeroConstraintLabelWriteAllocs is the MEASURED allocation budget of one
// zero-constraint label write through this harness. It is an absolute pin, and
// deliberately so.
//
// The first version of this test compared the operator carrying an empty registry
// against the same operator carrying none, on the theory that the driver's own
// allocations would cancel out. That control is BLIND to the regression that
// matters: work placed on the row path unconditionally — a reader built or a
// graph read issued outside the gate — lands in BOTH arms and the difference
// stays zero. Injecting exactly that (one `NodeLabels` call hoisted out of the
// gate) moved both arms 4 → 5 and the test still passed.
//
// An absolute budget sees it, because the number itself moves. It is safe to pin
// here: the harness is entirely local and deterministic — a one-row source and a
// map-backed mutator — and the figure measured 4, unchanged across repeated runs
// both with and without -race.
const zeroConstraintLabelWriteAllocs = 4

// TestLabelWrites_NoUniqueConstraint_AllocateNothingNew pins the allocation cost
// of a label write against a schema with no UNIQUE constraint. Any new per-row
// allocation — gated on the registry or not — moves the number and fails here.
//
// The differential against the pre-#2352 configuration (no registry attached at
// all) is kept as a second, narrower check: it names registry-gated work
// specifically, which makes a failure easier to read.
func TestLabelWrites_NoUniqueConstraint_AllocateNothingNew(t *testing.T) {
	cases := []struct {
		build func(*ConstraintRegistry) (Operator, *labelProbeMutator)
		name  string
	}{
		{name: "SetLabels", build: newSetLabelsProbe},
		{name: "RemoveLabels", build: newRemoveLabelsProbe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			measure := func(reg *ConstraintRegistry) float64 {
				op, _ := tc.build(reg)
				return testing.AllocsPerRun(200, func() {
					if err := driveOnce(op); err != nil {
						t.Fatalf("drive: %v", err)
					}
				})
			}
			baseline := measure(nil)                  // pre-#2352 configuration
			gated := measure(NewConstraintRegistry()) // registry attached, gate closed
			t.Logf("allocs/op: no registry (pre-#2352) = %v, empty registry (gate closed) = %v",
				baseline, gated)

			if gated != zeroConstraintLabelWriteAllocs {
				t.Fatalf("zero-constraint label write allocates %v/op, budget is %d — new per-row "+
					"work was added to the unconstrained path (see the constant's doc)",
					gated, zeroConstraintLabelWriteAllocs)
			}
			if gated != baseline {
				t.Fatalf("the registry-GATED enforcement path allocates: %v allocs/op with an "+
					"empty registry vs %v with no registry at all", gated, baseline)
			}
		})
	}
}
