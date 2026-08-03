package adjlist

// exclusive_build_test.go — the exclusive-build window's precondition, enforced
// rather than commented (rmp #2302, audit finding E21).
//
// [AdjList.BeginCommit] and [AdjList.BeginExclusiveBuild] mutate the same two
// plain fields. What separates them is entirely the licence they run under: the
// first is called by the serving write path with the graph's exclusive visibility
// barrier held, the second by store/recovery and store/bulkimport with NO barrier
// at all, licensed only by "the graph is not reachable by anyone yet".
//
// Both used to call BeginCommit, so that second licence lived in a comment. The
// audit's point is that it must not be silently INHERITED once writers overlap at
// serving time. These tests are what make it fail loudly instead.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// exclusiveRig builds a versioned AdjList with a clock attached, matching what
// recovery and bulk import hand it.
func exclusiveRig(t *testing.T) *AdjList[string, float64] {
	t.Helper()
	a := New[string, float64](Config{Directed: true, Multigraph: true})
	a.EnableVersioning()
	ws := &mvcc.WriteStamp{}
	ws.SetClock(&mvcc.Clock{})
	a.SetWriteStamp(ws)
	return a
}

// TestExclusiveBuild_RefusesAServingWindow is the enforcement, in the direction
// that matters most: a serving commit window opened while a rebuild is in flight.
//
// That is the case the audit warns about. Once the barrier is gone, a writer
// arriving mid-recovery would take the rebuild's bulkOwner as its own and mutate a
// shard's private, unpublished slot array in place. The panic makes it a bug
// report instead of a corrupted graph.
func TestExclusiveBuild_RefusesAServingWindow(t *testing.T) {
	a := exclusiveRig(t)
	a.BeginExclusiveBuild()
	defer a.EndExclusiveBuild()

	if !a.InExclusiveBuild() {
		t.Fatal("BeginExclusiveBuild did not mark the graph as being rebuilt")
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("BeginCommit succeeded during an exclusive build: the serving path and " +
				"the rebuild would share one builder-owner token, so one would mutate the " +
				"other's unpublished slot array in place")
		}
		if msg, ok := r.(string); !ok || msg == "" {
			t.Fatalf("panicked with %v, want a message naming the violated precondition", r)
		}
	}()
	a.BeginCommit()
}

// TestExclusiveBuild_RefusesEntryInsideAServingWindow covers the mirror
// direction, so neither mode can be entered from the other.
func TestExclusiveBuild_RefusesEntryInsideAServingWindow(t *testing.T) {
	a := exclusiveRig(t)
	a.BeginCommit()
	defer a.EndCommit()

	defer func() {
		if recover() == nil {
			t.Fatal("BeginExclusiveBuild succeeded inside a serving commit window; its " +
				"precondition is that nothing else is writing this graph")
		}
	}()
	a.BeginExclusiveBuild()
}

// TestExclusiveBuild_NestsAndReleases pins the shape recovery actually uses: one
// outer window around a whole replay, with nested opens inside it, and the flag
// cleared only on the outermost close — otherwise the serving path would be
// admitted while the rebuild was still running.
func TestExclusiveBuild_NestsAndReleases(t *testing.T) {
	a := exclusiveRig(t)

	a.BeginExclusiveBuild()
	a.BeginExclusiveBuild()
	if !a.InExclusiveBuild() {
		t.Fatal("flag not set inside a nested exclusive build")
	}
	a.EndExclusiveBuild()
	if !a.InExclusiveBuild() {
		t.Fatal("the inner close cleared the flag: the serving path would be admitted while " +
			"the rebuild is still running")
	}
	a.EndExclusiveBuild()
	if a.InExclusiveBuild() {
		t.Fatal("the outermost close did not clear the flag, so the graph is never handed " +
			"back to the serving path")
	}

	// And the serving path works again afterwards, which is what recovery hands
	// the engine when it returns.
	a.BeginCommit()
	a.EndCommit()
}

// TestExclusiveBuild_WritesStillLandInPlace guards against the enforcement having
// changed what the window is FOR. The clone-once-per-shard behaviour must be
// exactly what BeginCommit gave, since both mint the same kind of owner token.
func TestExclusiveBuild_WritesStillLandInPlace(t *testing.T) {
	a := exclusiveRig(t)
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge seed: %v", err)
	}
	id, ok := a.Mapper().Lookup("a")
	if !ok {
		t.Fatal("seed node absent")
	}
	sh := &a.shards[id&shardMask]

	a.BeginExclusiveBuild()
	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge in window: %v", err)
	}
	afterFirst := sh.slotsRef.Load()
	if err := a.AddEdge("a", "d", 1); err != nil {
		t.Fatalf("AddEdge in window: %v", err)
	}
	afterSecond := sh.slotsRef.Load()
	a.EndExclusiveBuild()

	if afterFirst != afterSecond {
		t.Fatal("the second write inside an exclusive build re-cloned the shard's slot " +
			"array; the window exists precisely to bound that to once per shard")
	}
	n := 0
	for range a.Neighbours("a") {
		n++
	}
	if n != 3 {
		t.Fatalf("node a has %d neighbours after the rebuild window, want 3", n)
	}
}
