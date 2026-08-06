package exec_test

// expand_intersect_test.go — correctness gates for the fused cyclic expand
// (rmp #2157).
//
// Layer: short.
//
// THE PRIMARY GATE IS A DIFFERENTIAL AGAINST THE PLAN THIS OPERATOR REPLACES.
// The reference is built from the shipped operators — Expand(b→c) then
// Expand(c→*) then keep the rows whose final destination is a — which is exactly
// what the engine plans today (an open Expand, a closing Expand and the equality
// Selection above it). Rows are compared as an ORDERED SEQUENCE, not as a
// multiset, because SPIKE #2155 identified emission order as a claim that must be
// asserted rather than reasoned about: if the sequence matches, no order-safety
// suppression is needed anywhere above.
//
// The fixtures deliberately include the two shapes the SPIKE proved a naive
// destination intersection gets WRONG: parallel edges (multiplicity is a
// cross-product of handle runs) and self-loops (one self-loop must not fill two
// legs of the same cycle).

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// orderedPair builds a forward CSR and its true transpose, with every run ordered
// ascending by destination.
//
// The ordering is not optional. buildCSR scatters in input order, and the
// Intersector — like every seek on the read path since #2141 — requires ordered
// runs; an unordered run does not return rows slowly, it returns the WRONG rows.
// Production builders (BuildFromAdjList, BuildFromAdjListLive, BuildReverse) all
// guarantee it, so a hand-assembled fixture must reproduce it or it is testing a
// shape no CSR reaching the executor can have.
func orderedPair(maxNode int, edgeList [][2]int) (fwd, rev *staticCSR) {
	build := func(list [][2]int) *staticCSR {
		perSrc := make([][]int, maxNode+1)
		for _, e := range list {
			perSrc[e[0]] = append(perSrc[e[0]], e[1])
		}
		verts := make([]uint64, maxNode+1)
		var edges []graph.NodeID
		for s := 0; s <= maxNode; s++ {
			verts[s] = uint64(len(edges))
			sort.Ints(perSrc[s])
			for _, d := range perSrc[s] {
				edges = append(edges, graph.NodeID(d))
			}
		}
		// verts must have maxNode+1 entries with verts[maxNode] == len(edges) so the
		// last source's window closes; sources are 0..maxNode-1.
		verts[maxNode] = uint64(len(edges))
		return &staticCSR{vertices: verts, edges: edges}
	}
	transposed := make([][2]int, 0, len(edgeList))
	for _, e := range edgeList {
		transposed = append(transposed, [2]int{e[1], e[0]})
	}
	return build(edgeList), build(transposed)
}

// referenceChain builds the plan the fused operator replaces: an open Expand for
// b→c, a second Expand for c→*, and the equality filter that keeps only the rows
// closing on a. Seed rows are [a, b], so the output layout is
// [a, b, b, r2, c, c, r3, dst] — the same eight columns the fused operator emits.
func referenceChain(t *testing.T, fwd, rev *staticCSR, seeds []exec.Row,
	midType, endType string, midFilter, endFilter map[uint64]string) []exec.Row {
	t.Helper()
	mid := exec.NewExpand(newSliceOperator(seeds...), exec.StaticAdjacency(fwd, rev, midFilter), exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  1, // b
		EdgeType:  midType,
	})
	closing := exec.NewExpand(mid, exec.StaticAdjacency(fwd, rev, endFilter), exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  4,        // c, appended by mid at base+2
		RelCols:   []int{3}, // r2, appended by mid at base+1
		EdgeType:  endType,
	})
	rows, err := exec.Drain(context.Background(), closing)
	if err != nil {
		t.Fatalf("reference chain drain: %v", err)
	}
	kept := make([]exec.Row, 0, len(rows))
	for _, r := range rows {
		if len(r) != 8 {
			t.Fatalf("reference row has %d columns, want 8: %v", len(r), r)
		}
		// The equality Selection: dst == a, under openCypher NODE IDENTITY rather
		// than Go interface equality.
		//
		// The distinction is invisible while every seed is a canonical
		// expr.IntegerValue — the two agree exactly — and decisive the moment a seed
		// arrives BOXED, as it does when the variable came through a projection.
		// `expr.IntegerValue(0) == expr.NodeValue{ID: 0}` is false in Go because the
		// dynamic types differ, so a `==` here silently drops every row of a boxed
		// fixture and makes this reference report zero. The engine's Selection does
		// not behave that way: [expr.NodeValue.Equal] compares on the underlying ID
		// precisely so a projected operand and an unprojected one still match.
		if sameNodeCell(r[7], r[0]) {
			kept = append(kept, r)
		}
	}
	return kept
}

// fusedRows drains the fused operator over the same seeds.
func fusedRows(t *testing.T, fwd, rev *staticCSR, seeds []exec.Row,
	midType, endType string, midFilter, endFilter map[uint64]string) []exec.Row {
	t.Helper()
	op := exec.NewExpandIntersect(newSliceOperator(seeds...), fwd, rev,
		&exec.ExpandIntersectConfig{
			MidCol:            1, // b
			EndCol:            0, // a
			MidEdgeType:       midType,
			EndEdgeType:       endType,
			MidEdgeTypeFilter: midFilter,
			EndEdgeTypeFilter: endFilter,
		})
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("fused drain: %v", err)
	}
	return rows
}

func rowText(r exec.Row) string {
	s := "["
	for i, v := range r {
		if i > 0 {
			s += " "
		}
		if iv, ok := v.(expr.IntegerValue); ok {
			s += itoaTest(int64(iv))
		} else {
			s += "?"
		}
	}
	return s + "]"
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// seedsFor builds one [a, b] row per edge a→b, which is what the upstream
// Expand(a→b) would have produced as the driving input for the fused hop.
func seedsFor(edgeList [][2]int) []exec.Row {
	seeds := make([]exec.Row, 0, len(edgeList))
	for _, e := range edgeList {
		seeds = append(seeds, exec.Row{expr.IntegerValue(e[0]), expr.IntegerValue(e[1])})
	}
	return seeds
}

func TestExpandIntersect_MatchesTheTwoExpandChain(t *testing.T) {
	cases := []struct {
		name    string
		maxNode int
		edges   [][2]int
	}{
		{"single directed triangle", 4, [][2]int{{0, 1}, {1, 2}, {2, 0}}},
		{"two triangles sharing an edge", 5, [][2]int{{0, 1}, {1, 2}, {2, 0}, {1, 3}, {3, 0}}},
		{"no triangle at all", 4, [][2]int{{0, 1}, {1, 2}}},
		{"parallel edges on the middle leg", 4,
			[][2]int{{0, 1}, {1, 2}, {1, 2}, {1, 2}, {2, 0}}},
		{"parallel edges on the closing leg", 4,
			[][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 0}}},
		{"parallel edges on both fused legs", 4,
			[][2]int{{0, 1}, {1, 2}, {1, 2}, {2, 0}, {2, 0}}},
		{"self-loop alongside a triangle", 4,
			[][2]int{{0, 1}, {1, 2}, {2, 0}, {0, 0}}},
		{"two parallel self-loops", 4,
			[][2]int{{0, 1}, {1, 2}, {2, 0}, {0, 0}, {0, 0}}},
		{"three parallel self-loops", 4,
			[][2]int{{0, 1}, {1, 2}, {2, 0}, {0, 0}, {0, 0}, {0, 0}}},
		{"dense little graph (every pair both ways)", 4,
			[][2]int{{0, 1}, {1, 0}, {1, 2}, {2, 1}, {0, 2}, {2, 0}}},
		{"hub fan-out", 7,
			[][2]int{{0, 1}, {1, 2}, {1, 3}, {1, 4}, {1, 5}, {2, 0}, {4, 0}, {5, 0}}},
		{"disconnected component present", 7,
			[][2]int{{0, 1}, {1, 2}, {2, 0}, {4, 5}, {5, 6}, {6, 4}}},
	}
	// totalRows guards against the differential going VACUOUSLY green. Two empty
	// result sets compare equal, so without this a bug that made the operator emit
	// nothing at all would pass every case above. Measured at 66 rows when written.
	totalRows := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fwd, rev := orderedPair(tc.maxNode, tc.edges)
			seeds := seedsFor(tc.edges)

			want := referenceChain(t, fwd, rev, seeds, "", "", nil, nil)
			got := fusedRows(t, fwd, rev, seeds, "", "", nil, nil)
			totalRows += len(got)

			if len(got) != len(want) {
				t.Fatalf("row count = %d, reference = %d\n  got:  %s\n  want: %s",
					len(got), len(want), rowsText(got), rowsText(want))
			}
			// ORDERED comparison: the sequence must match, not merely the multiset.
			for i := range want {
				if len(got[i]) != len(want[i]) {
					t.Fatalf("row %d width = %d, reference = %d", i, len(got[i]), len(want[i]))
				}
				for j := range want[i] {
					if got[i][j] != want[i][j] {
						t.Fatalf("row %d differs from the reference at column %d:\n  got  %s\n  want %s",
							i, j, rowText(got[i]), rowText(want[i]))
					}
				}
			}
		})
	}
	if totalRows < 60 {
		t.Fatalf("the whole differential produced only %d rows; it was 66 when written, "+
			"so it is no longer exercising the operator and has gone vacuously green", totalRows)
	}
}

func rowsText(rows []exec.Row) string {
	s := ""
	for _, r := range rows {
		s += rowText(r) + " "
	}
	if s == "" {
		return "(none)"
	}
	return s
}

// TestExpandIntersect_SelfLoopDoesNotFillTwoLegs is the case that breaks a naive
// destination intersection: it would bind a=b=c to the same self-loop for both
// fused legs. openCypher relationship isomorphism forbids reusing one edge, so a
// single self-loop must contribute nothing.
func TestExpandIntersect_SelfLoopDoesNotFillTwoLegs(t *testing.T) {
	// Node 0 has exactly one self-loop and no other cycle through it.
	fwd, rev := orderedPair(3, [][2]int{{0, 0}})
	seeds := []exec.Row{{expr.IntegerValue(0), expr.IntegerValue(0)}} // a=0, b=0
	got := fusedRows(t, fwd, rev, seeds, "", "", nil, nil)
	if len(got) != 0 {
		t.Fatalf("a single self-loop produced %d rows; want 0 (it cannot fill two legs): %s",
			len(got), rowsText(got))
	}
	// With TWO parallel self-loops the two legs can be distinct edges, so the
	// ordered pairs (r2, r3) with r2 != r3 are exactly 2.
	fwd2, rev2 := orderedPair(3, [][2]int{{0, 0}, {0, 0}})
	got2 := fusedRows(t, fwd2, rev2, seeds, "", "", nil, nil)
	if len(got2) != 2 {
		t.Fatalf("two parallel self-loops produced %d rows; want 2: %s", len(got2), rowsText(got2))
	}
	// And it must agree with the reference plan.
	want2 := referenceChain(t, fwd2, rev2, seeds, "", "", nil, nil)
	if len(got2) != len(want2) {
		t.Fatalf("two self-loops: fused %d rows, reference %d", len(got2), len(want2))
	}
}

// TestExpandIntersect_ParallelEdgeMultiplicity pins the cross-product rule with a
// hand-computed oracle rather than only against the reference plan, so a shared
// misunderstanding between the two implementations cannot hide.
func TestExpandIntersect_ParallelEdgeMultiplicity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mid, close int
		want       int
	}{
		{"1x1", 1, 1, 1},
		{"3x1", 3, 1, 3},
		{"1x2", 1, 2, 2},
		{"3x2", 3, 2, 6},
		{"2x2", 2, 2, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := [][2]int{{0, 1}}
			for i := 0; i < tc.mid; i++ {
				edges = append(edges, [2]int{1, 2})
			}
			for i := 0; i < tc.close; i++ {
				edges = append(edges, [2]int{2, 0})
			}
			fwd, rev := orderedPair(4, edges)
			seeds := []exec.Row{{expr.IntegerValue(0), expr.IntegerValue(1)}}
			got := fusedRows(t, fwd, rev, seeds, "", "", nil, nil)
			if len(got) != tc.want {
				t.Fatalf("mid=%d close=%d produced %d rows; hand-computed oracle says %d: %s",
					tc.mid, tc.close, len(got), tc.want, rowsText(got))
			}
		})
	}
}

// TestExpandIntersect_TypeFilterRestrictsBothLegs verifies each leg's filter is
// applied independently and on FORWARD positions.
func TestExpandIntersect_TypeFilterRestrictsBothLegs(t *testing.T) {
	// 0→1, 1→2, 2→0 plus a decoy 1→3, 3→0 forming a second triangle.
	edges := [][2]int{{0, 1}, {1, 2}, {1, 3}, {2, 0}, {3, 0}}
	fwd, rev := orderedPair(4, edges)
	seeds := []exec.Row{{expr.IntegerValue(0), expr.IntegerValue(1)}}

	// Admit every forward slot: both triangles must appear.
	all := map[uint64]string{}
	for i := range fwd.EdgesSlice() {
		all[uint64(i)] = "K"
	}
	both := fusedRows(t, fwd, rev, seeds, "K", "K", all, all)
	if len(both) != 2 {
		t.Fatalf("with every slot admitted, got %d rows; want 2: %s", len(both), rowsText(both))
	}
	if want := referenceChain(t, fwd, rev, seeds, "K", "K", all, all); len(want) != 2 {
		t.Fatalf("reference chain with every slot admitted gave %d rows; want 2", len(want))
	}

	// Now drop the middle leg 1→3 from the middle filter: only the 1→2 triangle
	// survives, and the fused result must still equal the reference.
	midOnly := map[uint64]string{}
	for i, d := range fwd.EdgesSlice() {
		if d == 3 {
			continue // the 1→3 slot
		}
		midOnly[uint64(i)] = "K"
	}
	got := fusedRows(t, fwd, rev, seeds, "K", "K", midOnly, all)
	want := referenceChain(t, fwd, rev, seeds, "K", "K", midOnly, all)
	if len(got) != len(want) {
		t.Fatalf("filtered middle leg: fused %d rows, reference %d\n  got %s\n want %s",
			len(got), len(want), rowsText(got), rowsText(want))
	}
	if len(got) != 1 {
		t.Fatalf("filtered middle leg produced %d rows; want 1: %s", len(got), rowsText(got))
	}
}

// TestExpandIntersect_SiblingMorphismExcludesAnEdge checks RelCols is honoured for
// both fused legs, which is what lets a sibling pattern in the same MATCH clause
// compete for edge identity.
func TestExpandIntersect_SiblingMorphismExcludesAnEdge(t *testing.T) {
	fwd, rev := orderedPair(4, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	// Seed carries a sibling edge id in column 2. Forward slot for 1→2 is index 1
	// (runs: node0 -> [1] at 0, node1 -> [2] at 1, node2 -> [0] at 2).
	seeds := []exec.Row{{expr.IntegerValue(0), expr.IntegerValue(1), expr.IntegerValue(1)}}
	op := exec.NewExpandIntersect(newSliceOperator(seeds...), fwd, rev,
		&exec.ExpandIntersectConfig{MidCol: 1, EndCol: 0, RelCols: []int{2}})
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("sibling column holding the middle edge did not exclude it; got %d rows: %s",
			len(rows), rowsText(rows))
	}
}

// TestExpandIntersect_EmptyLegsAndOutOfRangeNodes covers the short-circuit paths:
// a node with no out-edges, a node with no in-edges, and a node id beyond the
// snapshot's vertex space (a cached CSR pair can be narrower than the live node
// space). None may panic and all must yield nothing.
func TestExpandIntersect_EmptyLegsAndOutOfRangeNodes(t *testing.T) {
	fwd, rev := orderedPair(4, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	for _, tc := range []struct {
		name string
		a, b int64
	}{
		{"b has no out-edges", 0, 3},
		{"a has no in-edges", 3, 1},
		{"b far outside the vertex space", 0, 1 << 20},
		{"a far outside the vertex space", 1 << 20, 1},
		{"negative ids", -1, -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seeds := []exec.Row{{expr.IntegerValue(tc.a), expr.IntegerValue(tc.b)}}
			got := fusedRows(t, fwd, rev, seeds, "", "", nil, nil)
			if len(got) != 0 {
				t.Fatalf("got %d rows; want 0: %s", len(got), rowsText(got))
			}
		})
	}
}

// TestExpandIntersect_PlanHooks pins the Explain surface. PlanChildren is
// obligatory for any operator with inputs — TestPlanChildren_EveryOperatorWith
// InputsImplementsIt derives that obligation — and PlanDetail must state what the
// type name cannot.
func TestExpandIntersect_PlanHooks(t *testing.T) {
	fwd, rev := orderedPair(3, [][2]int{{0, 1}, {1, 0}})
	child := newSliceOperator()
	op := exec.NewExpandIntersect(child, fwd, rev, &exec.ExpandIntersectConfig{
		MidCol: 1, EndCol: 0, MidEdgeType: "KNOWS", EndEdgeType: "LIKES",
	})
	kids := op.PlanChildren()
	if len(kids) != 1 || kids[0] != child {
		t.Fatalf("PlanChildren = %v; want exactly the input operator", kids)
	}
	if d := op.PlanDetail(); d != "mid=KNOWS close=LIKES" {
		t.Fatalf("PlanDetail = %q; want %q", d, "mid=KNOWS close=LIKES")
	}
	untypedOp := exec.NewExpandIntersect(child, fwd, rev, &exec.ExpandIntersectConfig{})
	if d := untypedOp.PlanDetail(); d != "mid=* close=*" {
		t.Fatalf("untyped PlanDetail = %q; want %q", d, "mid=* close=*")
	}
}

// TestExpandIntersect_ReInitIsRepeatable is the regression gate for a defect this
// operator shipped with and that only an engine-level OPTIONAL MATCH test exposed:
// Init snapshotted the CSRs but did NOT reset the cursors, including done.
//
// Under a correlated Apply, Init runs once per OUTER ROW rather than once per query.
// So the first outer row ran the operator to exhaustion, left done=true, and every
// later outer row silently produced NOTHING — an OPTIONAL MATCH over a cyclic
// pattern returned a null row for all but at most the first input. It returned WRONG
// RESULTS without any error, which is the worst failure mode available.
//
// exec.Drain calls Init exactly once, so no amount of draining could have caught
// this; the operator has to be re-initialised explicitly, which is what this does.
func TestExpandIntersect_ReInitIsRepeatable(t *testing.T) {
	fwd, rev := orderedPair(4, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	seeds := []exec.Row{{expr.IntegerValue(0), expr.IntegerValue(1)}}
	op := exec.NewExpandIntersect(newSliceOperator(seeds...), fwd, rev,
		&exec.ExpandIntersectConfig{MidCol: 1, EndCol: 0})

	drainOnce := func(pass int) []exec.Row {
		if err := op.Init(context.Background()); err != nil {
			t.Fatalf("pass %d Init: %v", pass, err)
		}
		var rows []exec.Row
		for {
			var r exec.Row
			ok, err := op.Next(&r)
			if err != nil {
				t.Fatalf("pass %d Next: %v", pass, err)
			}
			if !ok {
				return rows
			}
			cp := make(exec.Row, len(r))
			copy(cp, r)
			rows = append(rows, cp)
		}
	}

	first := drainOnce(1)
	if len(first) == 0 {
		t.Fatal("the first pass produced no rows, so re-execution proves nothing")
	}
	second := drainOnce(2)
	if len(second) != len(first) {
		t.Fatalf("re-initialised operator produced %d rows; the first pass produced %d — "+
			"Init must reset every cursor, because a correlated Apply calls it once per OUTER ROW",
			len(second), len(first))
	}
	for i := range first {
		if len(second[i]) != len(first[i]) {
			t.Fatalf("pass 2 row %d width %d, pass 1 width %d", i, len(second[i]), len(first[i]))
		}
		for j := range first[i] {
			if second[i][j] != first[i][j] {
				t.Fatalf("pass 2 row %d differs at column %d:\n  pass1 %s\n  pass2 %s",
					i, j, rowText(first[i]), rowText(second[i]))
			}
		}
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
