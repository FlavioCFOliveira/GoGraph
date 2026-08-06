package exec

// expand_into_seek_test.go — white-box unit tests for the bound-destination cursor
// seek (#2151, for #2149).
//
// Layer: short.
//
// WHY WHITE-BOX. The seek is result- and order-IDENTICAL to the enumerate-and-filter
// path it replaces, by design. That is exactly what makes it invisible to a
// differential test: removing the narrowing leaves the operator walking the whole
// run, which is slower but returns the same rows in the same order. A mutation that
// deleted the reverse narrowing outright survived every end-to-end differential case
// in cypher/expand_into_seek_diff_test.go.
//
// So the narrowing must be asserted where it happens — on the cursor bounds after
// loadAdjacency — and that requires reaching unexported state. These tests are the
// only thing standing between a silent loss of the access path and nobody noticing.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// fakeCSR is a hand-assembled CSR satisfying CSRAdjacency, so a test states the
// adjacency it means rather than deriving it from a graph build.
type fakeCSR struct {
	verts   []uint64
	edges   []graph.NodeID
	handles []uint64
}

func (f *fakeCSR) VerticesSlice() []uint64    { return f.verts }
func (f *fakeCSR) EdgesSlice() []graph.NodeID { return f.edges }
func (f *fakeCSR) HandlesSlice() []uint64     { return f.handles }

// oneRowInput is a minimal Operator emitting a single supplied row once.
type oneRowInput struct {
	row  Row
	done bool
}

func (o *oneRowInput) Init(context.Context) error { return nil }
func (o *oneRowInput) Next(out *Row) (bool, error) {
	if o.done {
		return false, nil
	}
	o.done = true
	*out = o.row
	return true, nil
}
func (o *oneRowInput) Close() error { return nil }

// seekFixtureCSR builds the forward CSR for 3 nodes:
//
//	node 0 -> [1, 1, 1, 2]   (three PARALLEL edges to 1, then one to 2)
//	node 1 -> [0, 2]
//	node 2 -> []             (empty run)
//
// destination-ordered, with handles ascending inside each destination run, exactly as
// csr.OrderRuns produces.
func seekFixtureCSR() *fakeCSR {
	return &fakeCSR{
		verts:   []uint64{0, 4, 6, 6},
		edges:   []graph.NodeID{1, 1, 1, 2, 0, 2},
		handles: []uint64{10, 11, 12, 20, 30, 31},
	}
}

// reverseFixtureCSR is the transpose of seekFixtureCSR, (source, handle)-ordered:
//
//	node 0 <- [1]
//	node 1 <- [0, 0, 0]
//	node 2 <- [0, 1]
func reverseFixtureCSR() *fakeCSR {
	return &fakeCSR{
		verts:   []uint64{0, 1, 4, 6},
		edges:   []graph.NodeID{1, 0, 0, 0, 0, 1},
		handles: []uint64{30, 10, 11, 12, 20, 31},
	}
}

// cursorsAfterLoad drives one input row through Init/advanceInput and reports the
// cursor ranges the operator settled on.
func cursorsAfterLoad(t *testing.T, op *Expand, row Row) (fs, fe, rs, re uint64) {
	t.Helper()
	op.input = &oneRowInput{row: row}
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	done, err := op.advanceInput()
	if err != nil {
		t.Fatalf("advanceInput: %v", err)
	}
	if done {
		t.Fatal("advanceInput reported end-of-stream on the first row")
	}
	return op.fwdStart, op.fwdEnd, op.revStart, op.revEnd
}

// newSeekExpand builds an Expand over the fixture CSRs with source column 0 and the
// bound destination in column 1.
func newSeekExpand(dir Direction, seek bool) *Expand {
	op := NewExpand(nil, StaticAdjacency(seekFixtureCSR(), reverseFixtureCSR(), nil), ExpandConfig{
		Direction: dir,
		InputCol:  0,
	})
	return op.WithExpandInto(1).WithExpandIntoSeek(seek)
}

func TestSeekIntoRuns_ForwardCursorNarrowsToTheDestinationRun(t *testing.T) {
	// Source 0, bound destination 1: the run is positions [0,3) — all three parallel
	// edges, and NOT position 3, which lands on node 2.
	op := newSeekExpand(DirOut, true)
	fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(0), expr.IntegerValue(1)})
	if fs != 0 || fe != 3 {
		t.Fatalf("forward cursor = [%d,%d), want [0,3) — the whole parallel-edge run and "+
			"nothing beyond it", fs, fe)
	}
}

func TestSeekIntoRuns_ForwardCursorNarrowsToASingletonRun(t *testing.T) {
	// Source 0, bound destination 2: exactly position 3.
	op := newSeekExpand(DirOut, true)
	fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(0), expr.IntegerValue(2)})
	if fs != 3 || fe != 4 {
		t.Fatalf("forward cursor = [%d,%d), want [3,4)", fs, fe)
	}
}

func TestSeekIntoRuns_AbsentDestinationYieldsAnEmptyCursor(t *testing.T) {
	// Source 1 has no edge to 1, so the cursor must be empty — a MISS, not a range
	// that spills into a neighbouring node's slots.
	op := newSeekExpand(DirOut, true)
	fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(1), expr.IntegerValue(1)})
	if fs != fe {
		t.Fatalf("forward cursor = [%d,%d), want an empty range for an absent destination", fs, fe)
	}
	if !op.fwdDone {
		t.Fatal("fwdDone must be set once the seek empties the forward cursor")
	}
}

func TestSeekIntoRuns_EmptyAdjacencyStaysEmpty(t *testing.T) {
	// Source 2 has no out-edges at all.
	op := newSeekExpand(DirOut, true)
	fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(2), expr.IntegerValue(0)})
	if fs != fe {
		t.Fatalf("forward cursor = [%d,%d), want an empty range for an empty adjacency", fs, fe)
	}
}

func TestSeekIntoRuns_ReverseCursorNarrowsToTheSourceRun(t *testing.T) {
	// This is the case no end-to-end differential can see: node 1's reverse run is
	// [1,4) (three parallel in-edges from 0), and a bound destination of 0 must narrow
	// to exactly that, while a bound destination of 1 must yield an empty cursor.
	op := newSeekExpand(DirIn, true)
	_, _, rs, re := cursorsAfterLoad(t, op, Row{expr.IntegerValue(1), expr.IntegerValue(0)})
	if rs != 1 || re != 4 {
		t.Fatalf("reverse cursor = [%d,%d), want [1,4) — the three parallel in-edges from "+
			"node 0. A reverse seek that never narrows returns the same ROWS, so only this "+
			"assertion catches it", rs, re)
	}

	op2 := newSeekExpand(DirIn, true)
	_, _, rs2, re2 := cursorsAfterLoad(t, op2, Row{expr.IntegerValue(1), expr.IntegerValue(1)})
	if rs2 != re2 {
		t.Fatalf("reverse cursor = [%d,%d), want empty: node 1 has no in-edge from node 1", rs2, re2)
	}
}

func TestSeekIntoRuns_DirBothNarrowsBothCursors(t *testing.T) {
	// Node 1, bound destination 0: forward run [4,5) (the 1->0 edge) and reverse run
	// [1,4) (the three 0->1 edges).
	op := newSeekExpand(DirBoth, true)
	fs, fe, rs, re := cursorsAfterLoad(t, op, Row{expr.IntegerValue(1), expr.IntegerValue(0)})
	if fs != 4 || fe != 5 {
		t.Fatalf("forward cursor = [%d,%d), want [4,5)", fs, fe)
	}
	if rs != 1 || re != 4 {
		t.Fatalf("reverse cursor = [%d,%d), want [1,4)", rs, re)
	}
}

func TestSeekIntoRuns_DisabledLeavesTheFullRange(t *testing.T) {
	// With the seek off, the cursor must be the source's WHOLE run — the pre-#2149
	// behaviour and the "off" arm of every differential.
	op := newSeekExpand(DirBoth, false)
	fs, fe, rs, re := cursorsAfterLoad(t, op, Row{expr.IntegerValue(0), expr.IntegerValue(1)})
	if fs != 0 || fe != 4 {
		t.Fatalf("forward cursor = [%d,%d), want the full run [0,4) when the seek is disabled", fs, fe)
	}
	if rs != 0 || re != 1 {
		t.Fatalf("reverse cursor = [%d,%d), want the full run [0,1) when the seek is disabled", rs, re)
	}
}

// TestSeekIntoRuns_UndecidableCellKeepsTheFullRange is the guard that stops the seek
// from inventing a destination.
//
// If the bound cell cannot be resolved to a bare NodeID, boundIntoDst must report
// failure and the cursor must stay on the WHOLE run, leaving the operator's output a
// superset for the equality Selection above to decide. Dropping that guard would
// leave dst at its zero value and seek node 0's run — which for a boxed NodeValue
// naming any other node silently DROPS the rows the query needed, a wrong answer
// rather than a slow one.
func TestSeekIntoRuns_UndecidableCellKeepsTheFullRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		cell expr.Value
	}{
		{"null", expr.Null},
		{"boxed_node_value", expr.NodeValue{ID: 2}},
		{"string", expr.StringValue("2")},
		{"negative_id", expr.IntegerValue(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := newSeekExpand(DirOut, true)
			fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(0), tc.cell})
			if fs != 0 || fe != 4 {
				t.Fatalf("forward cursor = [%d,%d), want the full run [0,4): an unresolvable "+
					"bound cell must NOT be seeked, or the operator drops rows the Selection "+
					"above would have kept", fs, fe)
			}
			if _, ok := op.boundIntoDst(); ok {
				t.Fatalf("boundIntoDst resolved %s, which it must decline", tc.name)
			}
		})
	}
}

// TestSeekIntoRuns_NarrowRowKeepsTheFullRange covers a row shorter than intoCol.
func TestSeekIntoRuns_NarrowRowKeepsTheFullRange(t *testing.T) {
	op := newSeekExpand(DirOut, true)
	fs, fe, _, _ := cursorsAfterLoad(t, op, Row{expr.IntegerValue(0)})
	if fs != 0 || fe != 4 {
		t.Fatalf("forward cursor = [%d,%d), want the full run [0,4) for a row narrower than "+
			"the bound column", fs, fe)
	}
}
