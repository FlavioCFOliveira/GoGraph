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

// TestExclusiveBuild_AllowsAServingWindowNestedInside pins the behaviour a wrong
// guard rejected, so it cannot be re-added.
//
// The first version of this change panicked whenever a serving window was opened
// during a rebuild. `make ci` rejected it, in three packages, because recovery's
// own replay nests one on the SAME goroutine and on the dominant path: a replay
// creates versions fast enough to cross the reclamation threshold, and
// reclaimAfterDirectWrite runs the sweep inside an lpg.ApplyAtomically bracket,
// which opens a serving window.
//
// The hazard is CONCURRENCY, not nesting. Distinguishing them needs goroutine
// identity, which this package does not have — see BeginExclusiveBuild's doc for
// where that assertion belongs. So the nesting is allowed and counted.
func TestExclusiveBuild_AllowsAServingWindowNestedInside(t *testing.T) {
	a := exclusiveRig(t)
	a.BeginExclusiveBuild()

	before := a.NestedServingWindows()
	// This must NOT panic: it is exactly what recovery's reclamation sweep does.
	a.BeginCommit()
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge inside the nested serving window: %v", err)
	}
	a.EndCommit()

	if got := a.NestedServingWindows() - before; got != 1 {
		t.Fatalf("nested serving windows counted %d, want 1: the nesting is legitimate but "+
			"load-bearing (it is the reclamation sweep), so it has to stay observable", got)
	}
	if !a.InExclusiveBuild() {
		t.Fatal("closing the nested serving window ended the exclusive build")
	}

	a.EndExclusiveBuild()
	if a.InExclusiveBuild() {
		t.Fatal("the rebuild window did not close")
	}
	// The write made inside the nested window survived.
	n := 0
	for range a.Neighbours("a") {
		n++
	}
	if n != 1 {
		t.Fatalf("node a has %d neighbours, want 1: the nested window's write was lost", n)
	}
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

// TestBuilderOwner_TransactionKeepsCloneOnceWithNoBulkWindow pins clone-once with
// the TRANSACTION as the sole owner — no bulk window at all, which is the
// situation rmp #2304 leaves once the serving path's window is retired.
//
// # What it does NOT prove, checked rather than assumed
//
// It is NOT a regression gate for the ordering change in [AdjList.builderOwner].
// That was verified: with the old record-first ordering restored, this test still
// PASSES. The reason is that the first adjacency write of a transaction allocates
// the commit record itself, so by the time it asks for an owner the record already
// exists — the lazily-allocated nil window the old ordering guarded against never
// opens in this scenario.
//
// And that is consistent with the change's own claim: for ONE writer the two
// orderings behave identically. The reordering matters only with TWO concurrent
// writers, where the single per-AdjList bulk token would be shared and the
// per-transaction id would not — and the visibility barrier makes that
// unreachable today. So no single-writer test can discriminate, by construction,
// and this one is a guard on the property #2304 depends on rather than a
// discriminator between orderings.
func TestBuilderOwner_TransactionKeepsCloneOnceWithNoBulkWindow(t *testing.T) {
	a := exclusiveRig(t)
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge seed: %v", err)
	}
	id, ok := a.Mapper().Lookup("a")
	if !ok {
		t.Fatal("seed node absent")
	}
	sh := &a.shards[id&shardMask]

	// A transaction, and NO BeginCommit: the token can only come from the stamp.
	ws := a.WriteStampForTest()
	txID := beginTx(ws)
	if txID == 0 {
		t.Fatal("the test transaction got no id, so it cannot own a builder")
	}
	if got := ws.OpenTxID(); got != txID {
		t.Fatalf("OpenTxID = %d, want the armed transaction's id %d; without it the owner "+
			"is only available once the first version has allocated the record", got, txID)
	}

	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	afterFirst := sh.slotsRef.Load()
	if err := a.AddEdge("a", "d", 1); err != nil {
		t.Fatalf("second write: %v", err)
	}
	afterSecond := sh.slotsRef.Load()

	if afterFirst != afterSecond {
		t.Fatal("the transaction's second write to the same shard re-cloned its slot array. " +
			"The owner changed mid-transaction, which is the exact defect the old " +
			"bulk-window-first ordering existed to avoid — so OpenTxID is not delivering a " +
			"stable identity and the reordering in builderOwner is unsound")
	}

	info, _ := ws.End()
	if info == nil {
		t.Fatal("the transaction versioned nothing, so this test exercised no version chain")
	}
	info.Commit(ws.Clock().NextCommitTS())

	n := 0
	for range a.Neighbours("a") {
		n++
	}
	if n != 3 {
		t.Fatalf("node a has %d neighbours, want 3", n)
	}
}
