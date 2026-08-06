package exec

import (
	"context"
	"testing"
)

// This file is the regression gate for the type-filter defect rmp #2236's
// technical requirements ordered investigated FIRST, and which #2220 recorded on
// canBidirectional as "the forward-only reference returning a path whose hops use
// an edge the filter excludes".
//
// ROOT CAUSE: the reverse-to-forward position mapping reports "unresolved" by
// returning the reverse position ITSELF (resolvedFwdPosOrSelf), and
// passesTypeFilter reads that sentinel as `fwdPos == pos`. But a reverse slot's
// forward counterpart can legitimately SIT AT THE SAME absolute CSR position —
// with a single edge 0→1 it does, since both CSRs hold exactly one slot at index
// 0. In that case the mapping is perfectly well known, yet the check took the
// "cannot type-check, stay permissive" branch and admitted the edge WITHOUT
// consulting the filter.
//
// The sentinel conflated "I could not resolve this" with "I resolved it to the
// same index", and the second case is not rare: it is guaranteed whenever a
// reverse slot's forward twin lands on the same position.
//
// Since rmp #2236 widened canBidirectional these cases also cover the TWO-SIDED
// search: they drive bfsShortestPath, the dispatcher, so a typed search now
// reaches the two-sided algorithm and its own reverse-side admission test. The
// gate therefore guards both implementations against the same defect.

// typeFilterOperator builds a ShortestPath over g with a filter that admits only
// the forward positions listed in admit.
func typeFilterOperator(t *testing.T, g biTestGraph, dir Direction, admit ...uint64) *ShortestPath {
	t.Helper()
	fwd, rev := g.csrPair()
	filter := make(map[uint64]string, len(admit))
	for _, pos := range admit {
		filter[pos] = "K"
	}
	op := NewShortestPath(biNoInput{}, StaticAdjacency(fwd, rev, filter), dir, 0, 1)
	op.WithTypeFilter("K")
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return op
}

// admitsOnly reports membership in a set of forward positions. In both fixtures
// below every node has at most one outgoing edge, so a forward position and the
// edge's index in the original list coincide — which is what lets the same set
// serve as validatePath's admitted-edge predicate.
func admitsOnly(admit ...uint64) func(int) bool {
	set := make(map[int]struct{}, len(admit))
	for _, p := range admit {
		set[int(p)] = struct{}{}
	}
	return func(i int) bool {
		_, ok := set[i]
		return ok
	}
}

// TestShortestPath_TypeFilterRejectsAnExcludedReverseSlot is the reproduction and
// the gate. A single edge 0→1 that the filter EXCLUDES must yield no path, in
// every direction mode.
//
// The single-edge shape is the minimal case in which a reverse slot's forward
// counterpart shares its absolute position, which is what triggered the
// ambiguous sentinel. Before the fix, DirIn and DirBoth found a path over the
// excluded edge.
func TestShortestPath_TypeFilterRejectsAnExcludedReverseSlot(t *testing.T) {
	g := biTestGraph{2, [][2]int{{0, 1}}}

	for _, dir := range []struct {
		name string
		dir  Direction
		// src and dst are chosen so the single edge is the only candidate hop.
		src, dst uint64
	}{
		{"DirOut", DirOut, 0, 1},
		{"DirIn", DirIn, 1, 0},
		{"DirBoth", DirBoth, 0, 1},
	} {
		t.Run(dir.name+"/excluded", func(t *testing.T) {
			// An EMPTY admit list: no forward position is admitted, so no edge may
			// be traversed and there can be no path.
			op := typeFilterOperator(t, g, dir.dir)
			_, found, err := op.bfsShortestPath(dir.src, dir.dst)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if found {
				t.Errorf("a path was found over an edge the type filter excludes — the "+
					"reverse-slot check skipped the filter (dir %s)", dir.name)
			}
		})

		t.Run(dir.name+"/admitted", func(t *testing.T) {
			// The control: admitting the edge must find the path. Without this the
			// excluded case above would pass even if the search were broken
			// outright and never found anything.
			op := typeFilterOperator(t, g, dir.dir, 0)
			got, found, err := op.bfsShortestPath(dir.src, dir.dst)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if !found {
				t.Fatalf("no path found over an ADMITTED edge (dir %s) — the control is "+
					"broken, so the excluded case proves nothing", dir.name)
			}
			if l := pathLen(got); l != 1 {
				t.Errorf("path length %d, want 1 (dir %s)", l, dir.name)
			}
		})
	}
}

// TestShortestPath_TypeFilterPartialAdmissionAcrossDirections widens the
// reproduction past the minimal case: a chain where exactly one hop is excluded
// must be unroutable, in every direction, while the same chain with every hop
// admitted routes.
//
// A chain is used rather than a single edge so the search must reject a hop
// mid-walk rather than at the first step, which exercises the filter inside the
// frontier expansion rather than only at the seed.
func TestShortestPath_TypeFilterPartialAdmissionAcrossDirections(t *testing.T) {
	// 0→1→2→3, three edges at forward positions 0, 1, 2.
	g := biTestGraph{4, [][2]int{{0, 1}, {1, 2}, {2, 3}}}

	for _, dir := range []struct {
		name     string
		dir      Direction
		src, dst uint64
	}{
		{"DirOut", DirOut, 0, 3},
		{"DirIn", DirIn, 3, 0},
		{"DirBoth", DirBoth, 0, 3},
	} {
		t.Run(dir.name+"/middle hop excluded", func(t *testing.T) {
			op := typeFilterOperator(t, g, dir.dir, 0, 2) // 1 is missing
			_, found, err := op.bfsShortestPath(dir.src, dir.dst)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if found {
				t.Errorf("a path was found while the middle hop is excluded (dir %s)", dir.name)
			}
		})

		t.Run(dir.name+"/all hops admitted", func(t *testing.T) {
			op := typeFilterOperator(t, g, dir.dir, 0, 1, 2)
			fwd, _ := g.csrPair()
			got, found, err := op.bfsShortestPath(dir.src, dir.dst)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if !found {
				t.Fatalf("no path found with every hop admitted (dir %s)", dir.name)
			}
			if l := pathLen(got); l != 3 {
				t.Errorf("path length %d, want 3 (dir %s)", l, dir.name)
			}
			// Validated in EVERY direction, not only DirOut: hop resolution is keyed
			// off each predecessor entry's own CSR rather than off op.dir, so the
			// emitted orientation must be right for a reverse-read search too.
			validatePath(t, fwd, got, dir.src, dir.dst, admitsOnly(0, 1, 2))
		})
	}
}
