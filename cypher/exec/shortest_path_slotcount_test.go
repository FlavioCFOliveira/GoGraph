package exec

// shortest_path_slotcount_test.go — the exec-level gate on rmp #2763.
//
// [ShortestPath] and [AllShortestPaths] read relationship records across a BFS
// and, until #2763, reported none of it: they implemented neither
// [StorageRecordScan] nor storageAccessCounter, so their PROFILE cell rendered
// "?" (and, before rmp #2760, a bare 0 indistinguishable from a genuine zero).
//
// The engine-level gates in cypher/profile_dbhits_honesty_test.go assert what a
// PROFILE prints. This file asserts the three properties that a PROFILE cannot
// isolate:
//
//  1. the figure counts SLOTS READ, not arcs admitted — a slot the
//     relationship-type filter rejects is charged, because it was read before it
//     could be judged (the definition [Expand.storageAccesses] and Neo4j both
//     use);
//  2. the figure survives re-Init, so an operator driven once per outer row —
//     which is how a shortestPath whose endpoints come from an outer pattern is
//     planned, under a CorrelatedApply — reports its LIFETIME and not its last
//     invocation;
//  3. the count charged once per adjacency RUN equals the count a per-slot
//     increment would produce. That third one is not asserted here by a second
//     counter, because a committed second counter would cost every ordinary query
//     the thing this design refuses. It was established with a TEMPORARY per-slot
//     probe that panicked on disagreement (see [ShortestPath.scanRun]); what is
//     committed instead are the exact totals below, which a drifting charge
//     cannot keep satisfying.
//
// Every expected number here is derived from the algorithm in the comment above
// it and then checked against a CONTROL ARM in the same test — a second graph or
// a second query over the same shape whose slot count differs by a known amount
// while the ROWS do not move. A constant, an estimate, or a row-derived figure
// fails at least one arm of every case.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// slotCountOperator builds a ShortestPath over g and Inits it. filter is the set
// of forward positions the relationship-type filter admits; a nil filter means
// no type filter at all.
func slotCountOperator(t *testing.T, g biTestGraph, dir Direction, admit []uint64) *ShortestPath {
	t.Helper()
	fwd, rev := g.csrPair()
	var filter map[uint64]string
	if admit != nil {
		filter = make(map[uint64]string, len(admit))
		for _, pos := range admit {
			filter[pos] = "K"
		}
	}
	op := NewShortestPath(biNoInput{}, StaticAdjacency(fwd, rev, filter), dir, 0, 1)
	if filter != nil {
		op.WithTypeFilter("K")
	}
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return op
}

// slotCountAllOperator is [slotCountOperator] for AllShortestPaths.
func slotCountAllOperator(t *testing.T, g biTestGraph, dir Direction, admit []uint64) *AllShortestPaths {
	t.Helper()
	fwd, rev := g.csrPair()
	var filter map[uint64]string
	if admit != nil {
		filter = make(map[uint64]string, len(admit))
		for _, pos := range admit {
			filter[pos] = "K"
		}
	}
	op := NewAllShortestPaths(biNoInput{}, StaticAdjacency(fwd, rev, filter), dir, 0, 1)
	if filter != nil {
		op.WithTypeFilter("K")
	}
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return op
}

// broomEdges builds the fixture both operators are measured on: node 0 fans out
// to `fan` mids (nodes 1..fan), and mid 1 continues to the destination
// (node fan+1). Node ids are contiguous so the slice length is fan+2.
//
//	0 ──E──▶ 1 ──E──▶ fan+1        (the only path, length 2)
//	  ├─E──▶ 2
//	  ├─ …
//	  └─E──▶ fan
//
// The shape is chosen so the two searches have known frontiers: the forward one
// reads node 0's whole run (fan slots) and the backward one reads the
// destination's reverse run (1 slot). Nothing else is reachable, so nothing else
// is read.
func broomEdges(fan int) biTestGraph {
	edges := make([][2]int, 0, fan+1)
	for i := 1; i <= fan; i++ {
		edges = append(edges, [2]int{0, i})
	}
	edges = append(edges, [2]int{1, fan + 1})
	return biTestGraph{n: fan + 2, edges: edges}
}

// TestShortestPathSlotCount_CountsTheBFSFrontier is the exec-level form of the
// acceptance criterion: the figure equals the relationship slots the BFS
// enumerated.
//
// # The derivation, and the control arm that makes it falsifiable
//
// On [broomEdges](fan) the two-sided search runs:
//
//	levelF=0, levelB=0, |frontierF| = |frontierB| = 1 → expand FORWARD:
//	    biScan(0, fwd) reads node 0's whole run       = fan slots
//	    discovers mids 1..fan at distF = 1; no meet
//	|frontierF| = fan > |frontierB| = 1 → expand BACKWARD:
//	    biScan(fan+1, rev) reads the destination's run =   1 slot
//	    discovers mid 1 at distB = 1; distF[1] = 1 → best = 2
//	best(2) ≤ levelF(1) + levelB(1) → stop
//
// so the total is fan + 1, and the operator emits ONE path whatever fan is. Two
// arms with different fans are the control: the figure must move by exactly the
// number of slots added while the rows do not move at all. A figure derived from
// rows reports 1 for both; a constant reports the same for both; only a count of
// the slots actually walked tracks the arms.
func TestShortestPathSlotCount_CountsTheBFSFrontier(t *testing.T) {
	t.Parallel()

	type arm struct {
		fan  int
		want int64
	}
	arms := []arm{{fan: 100, want: 101}, {fan: 200, want: 201}, {fan: 7, want: 8}}

	measured := make([]int64, 0, len(arms))
	for _, a := range arms {
		op := slotCountOperator(t, broomEdges(a.fan), DirOut, nil)
		path, found, err := op.bfsShortestPath(0, uint64(a.fan+1))
		if err != nil {
			t.Fatalf("fan=%d: search: %v", a.fan, err)
		}
		if !found {
			t.Fatalf("fan=%d: no path found; the fixture is wrong, not the counter", a.fan)
		}
		// One path of length 2: [src, pos, mid, dir, pos, dst, dir] at stride 3.
		list, ok := path.(expr.ListValue)
		if !ok {
			t.Fatalf("fan=%d: path is %T, want expr.ListValue", a.fan, path)
		}
		if got := len(list); got != 1+2*VLEHopStride {
			t.Fatalf("fan=%d: path list has %d elements, want %d", a.fan, got, 1+2*VLEHopStride)
		}
		got := op.storageAccesses()
		if got != a.want {
			t.Errorf("fan=%d: storageAccesses() = %d, want %d "+
				"(node 0's run of %d slots plus the destination's reverse run of 1). "+
				"The operator emitted ONE path in every arm, so a figure that tracks "+
				"rows cannot produce this number and a constant cannot track the arms.",
				a.fan, got, a.want, a.fan)
		}
		measured = append(measured, got)
	}

	// The control: the delta between arms must be exactly the slots added.
	if d := measured[1] - measured[0]; d != int64(arms[1].fan-arms[0].fan) {
		t.Errorf("the figure moved by %d between fan=%d and fan=%d; %d relationship "+
			"slots were added and the emitted row count did not move at all",
			d, arms[0].fan, arms[1].fan, arms[1].fan-arms[0].fan)
	}
}

// TestShortestPathSlotCount_ChargesASlotTheTypeFilterRejected pins the
// DEFINITION, which is the half a total on its own cannot express: the figure
// counts slots READ, not arcs ADMITTED.
//
// Both arms walk the identical 8-slot run out of node 0. The untyped arm admits
// all 8; the typed one admits exactly the slot that continues to the destination.
// A figure counting admitted arcs would report 8 and 1; a figure counting slots
// read reports 9 and 9. That is [Expand.storageAccesses]'s definition, and
// Neo4j 5.26.16 charges the same way — RecordRelationshipTraversalCursor.next()
// calls tracer.onRelationship() inside the loop whose condition is the type
// selection, so a record read and then rejected is still counted.
func TestShortestPathSlotCount_ChargesASlotTheTypeFilterRejected(t *testing.T) {
	t.Parallel()

	const fan = 7
	g := broomEdges(fan) // forward positions 0..fan-1 are 0→i; position fan is 1→dst
	dst := uint64(fan + 1)

	untyped := slotCountOperator(t, g, DirOut, nil)
	if _, found, err := untyped.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("untyped: found=%v err=%v", found, err)
	}

	// Admit only the two forward positions on the actual path: 0→1 and 1→dst.
	typed := slotCountOperator(t, g, DirOut, []uint64{0, uint64(fan)})
	if _, found, err := typed.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("typed: found=%v err=%v", found, err)
	}

	// The forward-only fallback with the SAME filter. Its own type-filter branch
	// is a different line of code from the two-sided one, and a refund added to
	// either would be invisible to a gate that exercises only the other: mutating
	// spExpand's branch alone left every other assertion in this package green
	// (rmp #2763's mutation run, arm M4).
	fwd, _ := g.csrPair()
	filter := map[uint64]string{0: "K", uint64(fan): "K"}
	fallback := NewShortestPath(biNoInput{},
		StaticAdjacency(fwd, buildCSRWithHandles(g.n, nil), filter), DirOut, 0, 1)
	fallback.WithTypeFilter("K")
	if err := fallback.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if fallback.canBidirectional() {
		t.Fatalf("the fallback arm took the two-sided search; the control is void")
	}
	if _, found, err := fallback.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("typed fallback: found=%v err=%v", found, err)
	}

	gotU, gotT, gotF := untyped.storageAccesses(), typed.storageAccesses(), fallback.storageAccesses()
	if gotU != fan+1 || gotT != fan+1 || gotF != fan+1 {
		t.Errorf("untyped=%d typed(two-sided)=%d typed(forward-only)=%d, want %d for all three.\n"+
			"Every arm walks the SAME %d-slot run out of node 0 plus the same one "+
			"slot into the destination. A figure counting the arcs the type filter "+
			"ADMITTED would report %d for either typed arm; this column counts the slots "+
			"READ, whatever became of each one.",
			gotU, gotT, gotF, fan+1, fan, 2)
	}
}

// TestAllShortestPathsSlotCount_ChargesASlotTheTypeFilterRejected is
// [TestShortestPathSlotCount_ChargesASlotTheTypeFilterRejected] for
// AllShortestPaths, whose level-synchronous BFS has its own type-filter branch.
//
// It exists because a mutation proved the gate was needed rather than assumed:
// refunding a type-filtered slot inside aspExpand left every other assertion in
// this package and in cypher/profile_dbhits_honesty_test.go green (rmp #2763's
// mutation run, arm M4c). An operator whose definition nothing checks is an
// operator whose definition can drift.
func TestAllShortestPathsSlotCount_ChargesASlotTheTypeFilterRejected(t *testing.T) {
	t.Parallel()

	const fan = 7
	g := broomEdges(fan)
	dst := uint64(fan + 1)

	untyped := slotCountAllOperator(t, g, DirOut, nil)
	if paths, err := untyped.bfsAllShortest(0, dst); err != nil || len(paths) != 1 {
		t.Fatalf("untyped: %d paths, err=%v", len(paths), err)
	}
	typed := slotCountAllOperator(t, g, DirOut, []uint64{0, uint64(fan)})
	if paths, err := typed.bfsAllShortest(0, dst); err != nil || len(paths) != 1 {
		t.Fatalf("typed: %d paths, err=%v", len(paths), err)
	}

	gotU, gotT := untyped.storageAccesses(), typed.storageAccesses()
	if gotU != fan+1 || gotT != fan+1 {
		t.Errorf("untyped=%d typed=%d, want %d for both. The typed search admits 2 of "+
			"the %d slots it walks; a figure counting ADMITTED arcs would report 2.",
			gotU, gotT, fan+1, fan+1)
	}
}

// TestShortestPathSlotCount_LifetimeSurvivesReInit is the acceptance criterion
// "an operator driven once per outer row reports its whole lifetime, not the last
// invocation".
//
// Init is NOT called once per query. A shortestPath whose endpoints come from an
// outer pattern is planned under a CorrelatedApply, which re-Inits its inner plan
// for EVERY outer row (see [ShortestPath.revPrepared], which exists precisely
// because of that). A counter reset in Init would report the last row's search
// and call it the operator's cost.
//
// The oracle is a self-calibrating control: the second identical search must
// leave the total at exactly twice the first, and the third at exactly three
// times. That needs no hand-derived constant and fails on a reset (which would
// pin the total at 1x) as well as on a double charge (3x after two searches).
func TestShortestPathSlotCount_LifetimeSurvivesReInit(t *testing.T) {
	t.Parallel()

	const fan = 12
	g := broomEdges(fan)
	dst := uint64(fan + 1)

	op := slotCountOperator(t, g, DirOut, nil)
	if _, found, err := op.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("first search: found=%v err=%v", found, err)
	}
	first := op.storageAccesses()
	if first == 0 {
		t.Fatalf("the first search charged nothing; the fixture reads no slot at all")
	}

	for n := 2; n <= 3; n++ {
		if err := op.Init(context.Background()); err != nil {
			t.Fatalf("re-Init %d: %v", n, err)
		}
		if _, found, err := op.bfsShortestPath(0, dst); err != nil || !found {
			t.Fatalf("search %d: found=%v err=%v", n, found, err)
		}
		if got, want := op.storageAccesses(), first*int64(n); got != want {
			t.Fatalf("after %d identical searches storageAccesses() = %d, want %d "+
				"(%d per search). A figure that resets in Init reports %d however many "+
				"outer rows drove it.", n, got, want, first, first)
		}
	}
}

// TestAllShortestPathsSlotCount_CountsItsLevelSynchronousBFS is the
// AllShortestPaths half.
//
// This operator does NOT use the two-sided search — shortest_path_bidir.go's file
// comment says so explicitly, because reconstructing the multi-predecessor DAG
// across a meeting point is materially harder and the operator must stay
// level-synchronous. Its BFS on [broomEdges](fan) runs:
//
//	level 1: aspExpand(0) reads node 0's run          = fan slots
//	         discovers mids 1..fan at dist 1
//	level 2: aspExpand(1) reads mid 1's run           =   1 slot  → dst, foundLevel=2
//	         aspExpand(i>1) reads an empty run        =   0 slots
//	level 3: found && level > foundLevel → stop
//
// so the total is again fan + 1 — for ONE emitted path.
//
// The control arm is the second fixture, in which EVERY mid continues to the
// destination. There the operator emits `fan` paths and reads fan + fan slots. So
// across the two arms rows go 1 → fan while db-hits go fan+1 → 2·fan: a
// row-derived figure is wrong in both arms and wrong in opposite directions,
// which no single scaling factor can repair.
func TestAllShortestPathsSlotCount_CountsItsLevelSynchronousBFS(t *testing.T) {
	t.Parallel()

	const fan = 9

	t.Run("one mid reaches dst", func(t *testing.T) {
		t.Parallel()
		op := slotCountAllOperator(t, broomEdges(fan), DirOut, nil)
		paths, err := op.bfsAllShortest(0, uint64(fan+1))
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(paths) != 1 {
			t.Fatalf("got %d paths, want 1; the fixture is wrong", len(paths))
		}
		if got, want := op.storageAccesses(), int64(fan+1); got != want {
			t.Errorf("storageAccesses() = %d, want %d (node 0's %d-slot run plus mid 1's "+
				"one slot). The operator emitted 1 row, so a rows-derived figure reports 1.",
				got, want, fan)
		}
	})

	t.Run("every mid reaches dst", func(t *testing.T) {
		t.Parallel()
		// 0 → 1..fan, and every mid → dst.
		edges := make([][2]int, 0, 2*fan)
		for i := 1; i <= fan; i++ {
			edges = append(edges, [2]int{0, i})
		}
		for i := 1; i <= fan; i++ {
			edges = append(edges, [2]int{i, fan + 1})
		}
		op := slotCountAllOperator(t, biTestGraph{n: fan + 2, edges: edges}, DirOut, nil)
		paths, err := op.bfsAllShortest(0, uint64(fan+1))
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(paths) != fan {
			t.Fatalf("got %d paths, want %d; the fixture is wrong", len(paths), fan)
		}
		if got, want := op.storageAccesses(), int64(2*fan); got != want {
			t.Errorf("storageAccesses() = %d, want %d (node 0's %d-slot run plus one slot "+
				"per mid). The operator emitted %d rows here and 1 row in the sibling arm, "+
				"for walks of %d and %d slots: a rows-derived figure is wrong in both, in "+
				"opposite directions.", got, want, fan, fan, 2*fan, fan+1)
		}
	})
}

// TestAllShortestPathsSlotCount_LifetimeSurvivesReInit is
// [TestShortestPathSlotCount_LifetimeSurvivesReInit] for AllShortestPaths, and
// matters more here: this operator's Init resets four fields of per-input-row
// state, so a reset of the counter alongside them would look entirely natural.
func TestAllShortestPathsSlotCount_LifetimeSurvivesReInit(t *testing.T) {
	t.Parallel()

	const fan = 11
	g := broomEdges(fan)
	dst := uint64(fan + 1)

	op := slotCountAllOperator(t, g, DirOut, nil)
	if _, err := op.bfsAllShortest(0, dst); err != nil {
		t.Fatalf("first search: %v", err)
	}
	first := op.storageAccesses()
	if first == 0 {
		t.Fatalf("the first search charged nothing; the fixture reads no slot at all")
	}

	for n := 2; n <= 3; n++ {
		if err := op.Init(context.Background()); err != nil {
			t.Fatalf("re-Init %d: %v", n, err)
		}
		if _, err := op.bfsAllShortest(0, dst); err != nil {
			t.Fatalf("search %d: %v", n, err)
		}
		if got, want := op.storageAccesses(), first*int64(n); got != want {
			t.Fatalf("after %d identical searches storageAccesses() = %d, want %d "+
				"(%d per search)", n, got, want, first)
		}
	}
}

// TestShortestPathSlotCount_ForwardOnlyFallbackAlsoCounts covers the search the
// two-sided one falls back to. It is not the common path, but it is the path
// taken whenever the reverse CSR is unusable, and an uncounted fallback would
// make the figure depend on a property of the snapshot the reader cannot see —
// the same class of defect as the parallel threshold rmp #2762 removed.
//
// canBidirectional rejects a reverse CSR whose shape does not match the forward
// one, so a placeholder empty reverse forces the fallback. The forward-only walk
// reads node 0's whole run and then each mid's run:
//
//	level 1: spExpand(0)   = fan slots, discovers mids 1..fan
//	level 2: spExpand(1)   =   1 slot  → dst found
//	         spExpand(i>1) =   0 slots
//
// which is fan + 1 again — the same total the two-sided search reports for the
// same query on the same graph. That equality is the point: the figure must not
// move on an internal choice the reader did not make.
func TestShortestPathSlotCount_ForwardOnlyFallbackAlsoCounts(t *testing.T) {
	t.Parallel()

	const fan = 6
	g := broomEdges(fan)
	dst := uint64(fan + 1)

	twoSided := slotCountOperator(t, g, DirOut, nil)
	if !twoSided.canBidirectional() {
		t.Fatalf("the two-sided arm did not take the two-sided search; the control is void")
	}
	if _, found, err := twoSided.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("two-sided: found=%v err=%v", found, err)
	}

	// A placeholder reverse CSR: right vertex count, no edges. canBidirectional
	// rejects it on the O(1) shape check, so the search falls back.
	fwd, _ := g.csrPair()
	fallback := NewShortestPath(biNoInput{},
		StaticAdjacency(fwd, buildCSRWithHandles(g.n, nil), nil), DirOut, 0, 1)
	if err := fallback.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if fallback.canBidirectional() {
		t.Fatalf("the fallback arm took the two-sided search; the control is void")
	}
	if _, found, err := fallback.bfsShortestPath(0, dst); err != nil || !found {
		t.Fatalf("fallback: found=%v err=%v", found, err)
	}

	if got, want := fallback.storageAccesses(), int64(fan+1); got != want {
		t.Errorf("forward-only fallback storageAccesses() = %d, want %d", got, want)
	}
	if a, b := twoSided.storageAccesses(), fallback.storageAccesses(); a != b {
		t.Errorf("the two searches report %d and %d for the same query on the same graph; "+
			"the figure must not move on an internal choice the reader cannot see", a, b)
	}
}
