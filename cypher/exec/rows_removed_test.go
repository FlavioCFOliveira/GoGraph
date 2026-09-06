package exec

// rows_removed_test.go — white-box gates on the rows-removed-by-filter counter
// (rmp #2764).
//
// The engine-level gates live in cypher/profile_rows_removed_test.go, where a real
// PROFILE of a real query is read back from the rendered plan. This file is their
// package-internal complement, and it exists for four things those cannot reach:
//
//  1. rowsRemovedByFilter is UNEXPORTED, so only a test inside the package can read
//     the counter WITHOUT going through the renderer. A gate that can only see the
//     rendered string cannot tell a counter that is wrong from a renderer that is.
//  2. The counter must start at ZERO and be MOVED by the drain. Every arm asserts
//     the before-value as well as the after-value, so an operator whose counter was
//     pre-loaded by construction could not pass.
//  3. The figure must survive re-Init, because an operator under an Apply is
//     re-Init'd once per outer row and a reset there would report the last row's
//     rejections as the whole operator's.
//  4. The columnar and boxed paths of one ColumnarFilter must feed ONE counter,
//     which is only observable by driving the same object both ways.
//
// Exactness, rather than plausibility, is what each arm asserts: every input row is
// accounted for on both sides of `emitted + removed == pulled`, so an off-by-one or
// a halving fails, not merely a zeroing.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// drainNoClose Inits op and pulls it to exhaustion WITHOUT closing it, returning
// the rows emitted.
//
// [Drain] cannot be used where an operator is driven more than once: it Closes on
// the way out, and a Close releases the source (StaticRows drops its row slice), so
// a second Drain of the same tree emits nothing and a re-Init gate would assert
// against an empty pass. Re-Init WITHOUT Close is also the shape an Apply actually
// drives its inner plan in, which is the shape these gates exist to describe.
func drainNoClose(t *testing.T, op Operator) []Row {
	t.Helper()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out []Row
	for {
		var row Row
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, row)
	}
}

// removedRows builds n single-column rows holding the integers [0, n).
func removedRows(n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{expr.IntegerValue(i)}
	}
	return rows
}

// keepMultiplesOf returns a boxed predicate admitting rows whose integer is a
// multiple of k, and rejecting everything else with FALSE.
func keepMultiplesOf(k int64) FilterFn {
	return func(row Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv)%k == 0), nil
	}
}

// TestFilterRowsRemoved_IsExactAndStartsAtZero drives a Filter over a known input
// and asserts the counter against a number computed from the input, not against
// the operator's own arithmetic.
//
// 1000 rows, keep the multiples of 7: 143 survive (0, 7, … 994) and 857 are
// removed. Both halves are asserted, and their SUM is asserted against the input
// size — so a counter that halved, or that was off by one, fails on at least one of
// the three.
func TestFilterRowsRemoved_IsExactAndStartsAtZero(t *testing.T) {
	t.Parallel()
	const n = 1000
	f := NewFilter(NewStaticRows(removedRows(n)), keepMultiplesOf(7))

	if got := f.rowsRemovedByFilter(); got != 0 {
		t.Fatalf("a freshly built Filter reports rowsRemovedByFilter()=%d, want 0. "+
			"The counter must be MOVED by the drain, or every assertion below would "+
			"pass on a pre-loaded value", got)
	}

	rows, err := Drain(context.Background(), f)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	const wantKept = 143 // ceil(1000/7): 0, 7, … 994
	const wantRemoved = n - wantKept
	if len(rows) != wantKept {
		t.Fatalf("the filter emitted %d rows, want %d — the harness no longer "+
			"describes the input it thinks it does", len(rows), wantKept)
	}
	if got := f.rowsRemovedByFilter(); got != wantRemoved {
		t.Errorf("rowsRemovedByFilter()=%d, want %d. %d rows were pulled and %d "+
			"emitted, so exactly %d were removed",
			got, wantRemoved, n, wantKept, wantRemoved)
	}
	if got := int64(len(rows)) + f.rowsRemovedByFilter(); got != n {
		t.Errorf("emitted(%d) + removed(%d) = %d, want %d. Every row pulled from the "+
			"child leaves the loop through exactly one of those two exits, so this "+
			"identity is what makes the figure exact rather than plausible",
			len(rows), f.rowsRemovedByFilter(), got, n)
	}
}

// TestFilterRowsRemoved_NullIsARemoval pins the three-valued-logic half of the
// definition: openCypher 9 §4.1.3 drops a row on NULL exactly as it drops one on
// FALSE, so both are removals and a counter that only saw FALSE would under-report
// on every predicate over a missing property.
func TestFilterRowsRemoved_NullIsARemoval(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{expr.IntegerValue(1)},
		{expr.Null},
		{expr.IntegerValue(2)},
		{expr.Null},
	}
	f := NewFilter(NewStaticRows(rows), func(row Row) (expr.Value, error) {
		// NULL in, NULL out — the shape a property comparison takes on a node
		// missing the property.
		if _, ok := row[0].(expr.IntegerValue); !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(true), nil
	})
	out, err := Drain(context.Background(), f)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("emitted %d rows, want 2", len(out))
	}
	if got := f.rowsRemovedByFilter(); got != 2 {
		t.Errorf("rowsRemovedByFilter()=%d, want 2. A predicate returning NULL drops "+
			"the row (openCypher 9 §4.1.3), so the NULL rows are removals and must be "+
			"counted; a counter placed only on the explicit-FALSE branch reports %d", got, got)
	}
}

// TestFilterRowsRemoved_SurvivesReInit asserts the LIFETIME contract. An operator
// under an Apply is re-Init'd once per outer row, so a counter reset in Init would
// report the last row's rejections as the whole operator's — the same convention
// Expand.slotsRead and VarLengthExpand's traversal counter already follow.
func TestFilterRowsRemoved_SurvivesReInit(t *testing.T) {
	t.Parallel()
	const n = 100
	f := NewFilter(NewStaticRows(removedRows(n)), keepMultiplesOf(10))

	const wantKeptPerPass = 10 // 0, 10, … 90
	const wantRemovedPass = n - wantKeptPerPass

	for pass := 1; pass <= 3; pass++ {
		rows := drainNoClose(t, f)
		if len(rows) != wantKeptPerPass {
			t.Fatalf("pass %d emitted %d rows, want %d", pass, len(rows), wantKeptPerPass)
		}
		want := int64(pass * wantRemovedPass)
		if got := f.rowsRemovedByFilter(); got != want {
			t.Fatalf("after %d drain(s) rowsRemovedByFilter()=%d, want %d. The figure "+
				"is a LIFETIME total across re-Init; a counter reset in Init reports %d "+
				"here", pass, got, want, wantRemovedPass)
		}
	}
}

// TestColumnarFilterRowsRemoved_FillChunkPathCounts is the columnar arm. It drives
// FillChunk directly — the path a columnar parent takes, which never calls
// Filter.Next — so a counter placed only in the boxed loop would report 0 here.
//
// Both predicate paths are exercised, because they reject in different places: the
// unboxed ChunkPredicate decides most rows, and a nil ChunkPredicate forces every
// row through the boxed fallback inside FillChunk. The two must report the SAME
// figure over the same input, which is the columnar/boxed equivalence contract
// applied to the counter.
func TestColumnarFilterRowsRemoved_FillChunkPathCounts(t *testing.T) {
	t.Parallel()
	// The predicate keeps the multiples of 7 rather than the evens, DELIBERATELY:
	// an even/odd predicate rejects on strict alternation, so a mutation that
	// counted every other rejection would be inert against it and the gate would
	// pass on a halved counter. That mutation was run and is the reason this
	// number is 7 (rmp #2764). Multiples of 7 reject in runs of six.
	const n = 5000
	const wantKept = 715 // ceil(5000/7): 0, 7, … 4998
	const wantRemoved = n - wantKept

	unboxedSevens := func(src *Chunk, row int) (keep, decided bool) {
		v, valid := src.Int64(0, row)
		if !valid {
			return false, false
		}
		return v%7 == 0, true
	}
	boxedSevens := func(row Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv)%7 == 0), nil
	}

	for _, arm := range []struct {
		name string
		pred ChunkPredicate
	}{
		{"unboxed fast path", unboxedSevens},
		{"boxed fallback (nil ChunkPredicate)", nil},
	} {
		t.Run(arm.name, func(t *testing.T) {
			ids := make([]graph.NodeID, n)
			for i := range ids {
				ids[i] = graph.NodeID(i)
			}
			cf := NewColumnarFilter(NewAllNodesScan(&removedWalker{ids: ids}), boxedSevens, arm.pred)
			if got := cf.rowsRemovedByFilter(); got != 0 {
				t.Fatalf("a freshly built ColumnarFilter reports %d, want 0", got)
			}
			if err := cf.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			// maxRows is deliberately small and not a divisor of the batch size, so
			// filling stops mid-batch repeatedly and the scratch cursor persists
			// across calls. A counter bumped per BATCH rather than per row would
			// disagree here.
			dst := cf.NewOutputChunk(DefaultChunkCapacity)
			appended := 0
			for {
				k, err := cf.FillChunk(dst, 37)
				if err != nil {
					t.Fatalf("FillChunk: %v", err)
				}
				appended += k
				if k < 37 {
					break
				}
			}
			if appended != wantKept {
				t.Fatalf("appended %d rows, want %d", appended, wantKept)
			}
			if got := cf.rowsRemovedByFilter(); got != wantRemoved {
				t.Errorf("rowsRemovedByFilter()=%d, want %d. %d source rows were "+
					"examined and %d appended, so exactly %d were removed",
					got, wantRemoved, n, appended, wantRemoved)
			}
			if got := int64(appended) + cf.rowsRemovedByFilter(); got != n {
				t.Errorf("appended(%d) + removed(%d) = %d, want %d",
					appended, cf.rowsRemovedByFilter(), got, n)
			}
		})
	}
}

// TestColumnarFilterRowsRemoved_OneCounterForBothPaths drives ONE ColumnarFilter
// through its boxed Next path and then, after a re-Init, through its columnar
// FillChunk path, and asserts the two accumulate into a single figure.
//
// It is the counter's half of the reversibility contract the operator already
// documents: a ColumnarFilter whose parent turns out to consume it row-at-a-time
// runs Filter.Next, and the same object under a columnar parent runs FillChunk.
// Two counters would make the reported figure depend on which parent the planner
// happened to build.
func TestColumnarFilterRowsRemoved_OneCounterForBothPaths(t *testing.T) {
	t.Parallel()
	const n = 200
	const wantKeptPerPass = 29 // ceil(200/7): 0, 7, … 196
	const wantRemovedPerPass = n - wantKeptPerPass

	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	// Multiples of 7, for the reason given on the previous test: an alternating
	// predicate hides a counter that counts every other rejection.
	boxedSevens := func(row Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv)%7 == 0), nil
	}
	cf := NewColumnarFilter(NewAllNodesScan(&removedWalker{ids: ids}), boxedSevens,
		func(src *Chunk, row int) (keep, decided bool) {
			v, valid := src.Int64(0, row)
			if !valid {
				return false, false
			}
			return v%7 == 0, true
		})

	// Pass 1: the boxed Next path (a non-columnar parent).
	drainNoClose(t, cf)
	afterBoxed := cf.rowsRemovedByFilter()
	if afterBoxed != wantRemovedPerPass {
		t.Fatalf("after the boxed pass rowsRemovedByFilter()=%d, want %d",
			afterBoxed, wantRemovedPerPass)
	}

	// Pass 2: the columnar FillChunk path, on the SAME operator.
	if err := cf.Init(context.Background()); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	dst := cf.NewOutputChunk(DefaultChunkCapacity)
	for {
		k, err := cf.FillChunk(dst, 64)
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		if k < 64 {
			break
		}
	}
	if got, want := cf.rowsRemovedByFilter(), int64(2*wantRemovedPerPass); got != want {
		t.Errorf("after the boxed pass AND the columnar pass rowsRemovedByFilter()=%d, "+
			"want %d. Both paths must feed ONE counter: a ColumnarFilter reports the "+
			"same figure whichever parent drives it (it reported %d after the boxed "+
			"pass alone)", got, want, afterBoxed)
	}
}

// TestRowsRemoved_PlanNodeCarriesTheFigureAndItsFlag is the wiring gate: the
// counter must reach [PlanNode] through the profiling wrapper, with the flag that
// says whether it is a figure at all.
//
// The two arms are the whole tri-state in one test — a Filter reports (997, true),
// its scan child reports (0, false) — and the second is the one that matters: an
// operator that removes no rows must be distinguishable from one that removed none,
// which is what OMITTING the cell rather than printing 0 buys (rmp #2764,
// following rmp #2760's rule for db-hits).
func TestRowsRemoved_PlanNodeCarriesTheFigureAndItsFlag(t *testing.T) {
	t.Parallel()
	const n = 1000
	const wantKept = 3

	p := NewProfiler()
	scan := p.Wrap(NewStaticRows(removedRows(n)))
	root := p.Wrap(NewFilter(scan, func(row Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv) < wantKept), nil
	}))
	rows, err := Drain(context.Background(), root)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != wantKept {
		t.Fatalf("emitted %d rows, want %d", len(rows), wantKept)
	}

	tree := PlanTree(root)
	if tree.Name != "Filter" {
		t.Fatalf("root node is %q, want Filter", tree.Name)
	}
	if !tree.RowsRemovedByFilterKnown {
		t.Fatalf("the Filter's RowsRemovedByFilterKnown is false. It implements " +
			"rowsRemovedCounter, so its figure exists and must be rendered")
	}
	if got, want := tree.RowsRemovedByFilter, int64(n-wantKept); got != want {
		t.Errorf("PlanNode.RowsRemovedByFilter=%d, want %d", got, want)
	}
	if len(tree.Children) != 1 {
		t.Fatalf("the Filter has %d children, want 1", len(tree.Children))
	}
	child := tree.Children[0]
	if child.RowsRemovedByFilterKnown {
		t.Errorf("%s reports RowsRemovedByFilterKnown=true with figure %d. A row "+
			"source removes no rows, so it must report NO figure — the renderers omit "+
			"the cell on that flag, and a true here would make them print a 0 the "+
			"operator never earned", child.Name, child.RowsRemovedByFilter)
	}

	// And the rendering itself, which is the surface a reader sees.
	out := RenderPlanNode(&tree)
	if !containsLine(out, "Filter", "removed=997") {
		t.Errorf("the rendered Filter line carries no `removed=997`:\n%s", out)
	}
	if containsLine(out, "StaticRows", "removed=") {
		t.Errorf("the rendered StaticRows line carries a `removed=` cell. An operator "+
			"that removes no rows must OMIT the figure, never print 0 for it:\n%s", out)
	}
}

// TestRowsRemovedCell_BlankIsNotZero pins the table cell renderer, whose whole job
// is the distinction the flag carries.
func TestRowsRemovedCell_BlankIsNotZero(t *testing.T) {
	t.Parallel()
	if got := RowsRemovedCell(0, false); got != "" {
		t.Errorf("RowsRemovedCell(0, false) = %q, want \"\" — an operator that removes "+
			"no rows has no figure and its cell is blank", got)
	}
	if got := RowsRemovedCell(0, true); got != "0" {
		t.Errorf("RowsRemovedCell(0, true) = %q, want \"0\" — a filter that rejected "+
			"nothing MEASURED a zero, and that is the finding a reader of a slow plan "+
			"wants. GoGraph diverges from PostgreSQL here, which suppresses a zero "+
			"count in text mode (explain.c:3638)", got)
	}
	if got := RowsRemovedCell(997, true); got != "997" {
		t.Errorf("RowsRemovedCell(997, true) = %q, want \"997\"", got)
	}
}

// removedWalker is a NodeIDWalker over a fixed id list, for the columnar arms.
type removedWalker struct {
	ids []graph.NodeID
}

func (w *removedWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

// containsLine reports whether some line of out names operator and carries needle.
func containsLine(out, operator, needle string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, operator) && strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// TestExpandRowsRemoved_ExpandIntoComparison covers the fourth of Expand's
// rejection branches — the bound-destination comparison [Expand.dstMatchesInto] —
// and, in the same two arms, CHECKS A CLAIM this package's documentation makes
// about it.
//
// The branch is only reachable when the expand-into SEEK is off. With the seek on,
// which is the default whenever a destination is bound, [Expand.seekIntoRuns]
// narrows the cursor to the destination's contiguous run BEFORE the walk begins, so
// the comparison never rejects anything and the slots it skipped are not read (and
// so are charged to neither counter). [Expand.rowsRemovedByFilter] states that; this
// asserts it, rather than leaving a documented "rejects nothing" to be believed.
//
// The fixture is the seek suite's: node 0 -> [1, 1, 1, 2]. Bound to destination 2:
//
//	seek OFF — 4 slots walked, 3 rejected by the comparison, 1 emitted
//	seek ON  — 1 slot walked, 0 rejected, 1 emitted
//
// The reverse arm uses node 2's in-edge run [0, 1] bound to source 0, which is the
// only place the REVERSE comparison can fire; the engine cannot reach it at all,
// because a bound destination always arrives with the seek enabled.
func TestExpandRowsRemoved_ExpandIntoComparison(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name        string
		dir         Direction
		row         Row
		seek        bool
		wantRows    int
		wantWalked  int64
		wantRemoved int64
	}{
		// node 0 -> [1,1,1,2]; bound destination 2 is the LAST slot.
		{"forward, seek off", DirOut, Row{expr.IntegerValue(0), expr.IntegerValue(2)}, false, 1, 4, 3},
		{"forward, seek on", DirOut, Row{expr.IntegerValue(0), expr.IntegerValue(2)}, true, 1, 1, 0},
		// node 2 <- [0, 1]; bound source 0 admits one of the two.
		{"reverse, seek off", DirIn, Row{expr.IntegerValue(2), expr.IntegerValue(0)}, false, 1, 2, 1},
		{"reverse, seek on", DirIn, Row{expr.IntegerValue(2), expr.IntegerValue(0)}, true, 1, 1, 0},
	} {
		t.Run(arm.name, func(t *testing.T) {
			op := newSeekExpand(arm.dir, arm.seek)
			op.input = &oneRowInput{row: arm.row}
			rows := drainNoClose(t, op)

			if len(rows) != arm.wantRows {
				t.Fatalf("emitted %d rows, want %d — the fixture no longer produces the "+
					"walk this arm describes", len(rows), arm.wantRows)
			}
			if got := op.storageAccesses(); got != arm.wantWalked {
				t.Fatalf("walked %d slots, want %d", got, arm.wantWalked)
			}
			if got := op.rowsRemovedByFilter(); got != arm.wantRemoved {
				t.Errorf("rowsRemovedByFilter()=%d, want %d. The expand-into comparison "+
					"is the only branch that can reject here, and with the seek %v it "+
					"must reject %d of the %d slots walked",
					got, arm.wantRemoved, arm.seek, arm.wantRemoved, arm.wantWalked)
			}
			if got := int64(len(rows)) + op.rowsRemovedByFilter(); got != op.storageAccesses() {
				t.Errorf("rows(%d) + removed(%d) = %d against %d slots walked. Every slot "+
					"consumed is either emitted or rejected",
					len(rows), op.rowsRemovedByFilter(), got, op.storageAccesses())
			}
		})
	}
}
