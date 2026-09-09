package index

import (
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// build_resolver_test.go — rmp #2793 gate: a build log replays the resolution
// captured WHEN THE CHANGE WAS FANNED OUT, not one taken later.
//
// # The defect these tests pin
//
// A bound index's Apply answers two questions about the changed node from the
// GRAPH — is it eligible for this index, and what is its current value of the
// bound property — because a [Change] carries neither. [Manager.FinishBuild]
// applies a recorded change long after it was recorded, so asking those
// questions during the replay answers about a later instant, and the graph at
// that instant carries every open transaction's eager, uncommitted writes.
//
// These tests hold the graph side abstract: the resolver is a function whose
// answers CHANGE between the recording and the replay, which is exactly what an
// interfering transaction does to the real one. What must be observable is that
// the replay carries the recorded answer and not the later one.

// resolvedSub is a [ResolvedApplier] that records how each change reached it, so
// a test can assert not merely the content but the ROUTE and the payload.
type resolvedSub struct {
	plain    []Change
	resolved []resolvedCall
	mu       sync.Mutex
}

// resolvedCall is one ApplyResolved delivery.
type resolvedCall struct {
	change   Change
	current  any
	eligible bool
}

func (r *resolvedSub) Apply(c Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plain = append(r.plain, c)
}

func (r *resolvedSub) ApplyResolved(c Change, current any, eligible bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = append(r.resolved, resolvedCall{change: c, current: current, eligible: eligible})
}

func (r *resolvedSub) Kind() string { return "resolved" }

func (r *resolvedSub) calls() ([]Change, []resolvedCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plain, r.resolved
}

// TestBuildLog_ReplayCarriesTheResolutionTakenWhenTheChangeWasFannedOut is the
// core claim of rmp #2793: the answer the replay applies is the one the resolver
// gave AT RECORD TIME, even though the same resolver would now answer
// differently.
//
// The resolver's answers are flipped after the recording and before the replay,
// which is what a second transaction's eager, uncommitted mutation does to the
// live graph. If the log re-resolved — or stored a reference rather than a value
// — the replay would carry the flipped answer, and this test would see it.
func TestBuildLog_ReplayCarriesTheResolutionTakenWhenTheChangeWasFannedOut(t *testing.T) {
	t.Parallel()
	m := NewManager()

	// The state the resolver reads. It stands in for the live graph.
	current := any("committed")
	eligible := true
	calls := 0
	bl := m.BeginBuild(func(Change) (any, bool) {
		calls++
		return current, eligible
	})

	m.Apply(setProp(7))
	m.ApplyBatch([]Change{setProp(8), setProp(9)})
	if calls != 3 {
		t.Fatalf("the resolver was consulted %d times for 3 recorded changes, want 3 — "+
			"a change recorded without resolution is a change that will be re-resolved "+
			"at replay", calls)
	}

	// The interference: the graph now says something else entirely.
	current, eligible = any("uncommitted-ghost"), false

	fresh := &resolvedSub{}
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", fresh) }); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}

	plain, resolved := fresh.calls()
	if len(plain) != 0 {
		t.Fatalf("the replay reached Apply %d times: a subscriber that can take the recorded "+
			"resolution must never be handed a change to re-resolve", len(plain))
	}
	if len(resolved) != 3 {
		t.Fatalf("the replay delivered %d resolved changes, want 3", len(resolved))
	}
	for k, want := range []graph.NodeID{7, 8, 9} {
		if resolved[k].change.Node != want {
			t.Errorf("replay[%d] is node %d, want %d (mutation order must be preserved)",
				k, resolved[k].change.Node, want)
		}
		if got, ok := resolved[k].current.(string); !ok || got != "committed" {
			t.Errorf("replay[%d] carries current=%v, want %q — the replay re-resolved against "+
				"the state at replay time instead of carrying the state the change was "+
				"recorded with", k, resolved[k].current, "committed")
		}
		if !resolved[k].eligible {
			t.Errorf("replay[%d] carries eligible=false, want true — same defect, through the "+
				"eligibility door", k)
		}
	}
	// The registration itself still happened: without this the assertions above
	// would hold just as well for a FinishBuild that registered nothing.
	if _, err := m.GetIndex("built"); err != nil {
		t.Fatalf("GetIndex after FinishBuild: %v", err)
	}
	// And the live fan-out still goes through Apply, unresolved — the recorded
	// route is for the replay only.
	m.Apply(setProp(10))
	plain, _ = fresh.calls()
	if len(plain) != 1 || plain[0].Node != 10 {
		t.Fatalf("the live fan-out after FinishBuild delivered %v through Apply, want exactly "+
			"node 10: a registered index must be maintained from the graph, not from a "+
			"stale recording", plain)
	}
}

// TestBuildLog_NilResolverReplaysThroughApply pins the documented fallback: a
// log with no resolver has nothing to hand a [ResolvedApplier], so it replays
// through [Subscriber.Apply] exactly as it did before rmp #2793.
//
// It is the contract every caller that registers a subscriber resolving nothing
// from the graph relies on, and it is asserted rather than assumed because the
// alternative — handing over a zero resolution — would silently suppress every
// insert such a replay should make.
func TestBuildLog_NilResolverReplaysThroughApply(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild(nil)
	m.Apply(setProp(7))

	fresh := &resolvedSub{}
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", fresh) }); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	plain, resolved := fresh.calls()
	if len(resolved) != 0 {
		t.Fatalf("a resolver-less log delivered %d changes as RESOLVED, want 0: it has no "+
			"resolution to deliver", len(resolved))
	}
	if len(plain) != 1 || plain[0].Node != 7 {
		t.Fatalf("a resolver-less log replayed %v through Apply, want exactly node 7", plain)
	}
}

// TestBuildLog_PlainSubscriberStillReplaysThroughApply covers the other half of
// the choice: a resolver is present, but the subscriber cannot take a
// resolution. It must still be caught up (rmp #2738), through the only method it
// has.
func TestBuildLog_PlainSubscriberStillReplaysThroughApply(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild(func(Change) (any, bool) { return "v", true })
	m.Apply(setProp(7))

	fresh := &recordSub{}
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", fresh) }); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	if got := fresh.nodes(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("a subscriber that cannot take a resolution was caught up with %v, want "+
			"exactly node 7 — the catch-up of rmp #2738 must not depend on the new route", got)
	}
}

// TestBuildLog_OverflowStopsConsultingTheResolver pins the cost claim on
// [BuildLog.appendLocked]: past the ceiling the log is already useless, so it
// must stop paying for graph reads it will never replay.
//
// The ceiling is reached with the real constant rather than a lowered one, so the
// number under test is the number that ships.
func TestBuildLog_OverflowStopsConsultingTheResolver(t *testing.T) {
	t.Parallel()
	m := NewManager()
	calls := 0
	bl := m.BeginBuild(func(Change) (any, bool) {
		calls++
		return nil, false
	})
	for k := 0; k <= MaxBuildLogChanges; k++ {
		m.Apply(setProp(graph.NodeID(k)))
	}
	if !bl.Overflowed() {
		t.Fatalf("the log did not overflow after %d changes", MaxBuildLogChanges+1)
	}
	if calls != MaxBuildLogChanges {
		t.Errorf("the resolver was consulted %d times for %d recorded changes plus one that "+
			"overflowed, want %d — a change the log discards must not cost a graph read",
			calls, MaxBuildLogChanges, MaxBuildLogChanges)
	}
	before := calls
	m.Apply(setProp(1))
	if calls != before {
		t.Errorf("the resolver was consulted again after the overflow latched (%d -> %d)",
			before, calls)
	}
	err := m.FinishBuild(bl, func(RegisterFunc) error {
		t.Error("FinishBuild ran the registration closure for an overflowed log")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "overflowed") {
		t.Fatalf("FinishBuild on an overflowed log = %v, want ErrIndexBuildOverflow", err)
	}
}
