package exec_test

// project_rowbinder_test.go — the driver half of rmp #2658.
//
// The engine builds one shared binding context per input row; this file pins the
// contract the DRIVERS owe it, independently of what the engine does with it:
//
//   - exactly one BindRow and one ReleaseRow per input row, on BOTH row-at-a-time
//     drivers ([exec.Project.Next] and the row-input arm of
//     [exec.ColumnarProject.FillChunk]);
//   - every item of one row evaluated INSIDE that bracket;
//   - the bracket closed on the error exits too — an item error and the per-row
//     byte budget — because an unclosed bracket strands a pooled binding map and
//     its value arena for the life of the process;
//   - no bracket at all when no binder is installed, so the un-fused build is
//     byte-identical.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// recordingBinder counts the bracket calls and records, per item evaluation,
// whether a row was bound at the time — which is what proves the items run INSIDE
// the bracket rather than merely alongside it.
type recordingBinder struct {
	binds    int
	releases int
	bound    bool
	// maxDepth catches a second BindRow before the matching ReleaseRow: the binder
	// holds one row's state, so nesting would silently discard a row's context.
	maxDepth int
	depth    int
}

func (r *recordingBinder) BindRow(_ exec.Row) {
	r.binds++
	r.bound = true
	r.depth++
	if r.depth > r.maxDepth {
		r.maxDepth = r.depth
	}
}

func (r *recordingBinder) ReleaseRow() {
	r.releases++
	r.bound = false
	r.depth--
}

// boundProbeItems returns n items that each record whether a row was bound when
// they ran, plus the slice those observations land in.
func boundProbeItems(n int, r *recordingBinder) ([]exec.ProjectionItem, *[]bool) {
	seen := make([]bool, 0, n)
	items := make([]exec.ProjectionItem, n)
	for i := 0; i < n; i++ {
		items[i] = exec.ProjectionItem{
			Alias: "c" + string(rune('a'+i)),
			Eval: func(_ exec.Row) (expr.Value, error) {
				seen = append(seen, r.bound)
				return expr.IntegerValue(1), nil
			},
		}
	}
	return items, &seen
}

func TestProjectBracketsEachRowExactlyOnce(t *testing.T) {
	const rows, cols = 3, 2
	binder := &recordingBinder{}
	items, seen := boundProbeItems(cols, binder)

	src := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
	)
	p, err := exec.NewProject(src, items)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	p.WithRowBinder(binder)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n := 0
	var out exec.Row
	for {
		ok, err := p.Next(&out)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		n++
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if n != rows {
		t.Fatalf("projected %d rows, want %d", n, rows)
	}
	if binder.binds != rows || binder.releases != rows {
		t.Fatalf("binds=%d releases=%d over %d rows, want %d of each: the shared context must be "+
			"built once per ROW, not per column and not once per query",
			binder.binds, binder.releases, rows, rows)
	}
	if binder.maxDepth != 1 {
		t.Fatalf("bracket nesting depth reached %d: a second row was bound before the first was "+
			"released, so one row's context was discarded unreleased", binder.maxDepth)
	}
	if len(*seen) != rows*cols {
		t.Fatalf("%d item evaluations, want %d (%d rows x %d columns)", len(*seen), rows*cols, rows, cols)
	}
	for i, wasBound := range *seen {
		if !wasBound {
			t.Fatalf("item evaluation %d ran with NO row bound: it would build its own context, "+
				"which is the cost this seam removes", i)
		}
	}
}

func TestProjectReleasesTheRowOnAnItemError(t *testing.T) {
	binder := &recordingBinder{}
	boom := errors.New("item exploded")
	items := []exec.ProjectionItem{
		{Alias: "ok", Eval: func(_ exec.Row) (expr.Value, error) { return expr.IntegerValue(1), nil }},
		{Alias: "bad", Eval: func(_ exec.Row) (expr.Value, error) { return nil, boom }},
	}
	p, err := exec.NewProject(newSliceOperator(exec.Row{expr.IntegerValue(1)}), items)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	p.WithRowBinder(binder)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out exec.Row
	if _, err := p.Next(&out); !errors.Is(err, boom) {
		t.Fatalf("Next error = %v, want %v", err, boom)
	}
	if binder.binds != 1 || binder.releases != 1 {
		t.Fatalf("binds=%d releases=%d after a failed item, want 1 of each: an unclosed bracket "+
			"strands the pooled binding map and its value arena permanently",
			binder.binds, binder.releases)
	}
}

func TestProjectReleasesTheRowWhenTheByteBudgetTrips(t *testing.T) {
	binder := &recordingBinder{}
	items := []exec.ProjectionItem{
		{Alias: "big", Eval: func(_ exec.Row) (expr.Value, error) { return expr.IntegerValue(1), nil }},
		{Alias: "unreached", Eval: func(_ exec.Row) (expr.Value, error) { return expr.IntegerValue(2), nil }},
	}
	p, err := exec.NewProject(newSliceOperator(exec.Row{expr.IntegerValue(1)}), items)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	// A ceiling of 1 with a per-value estimate of 10 trips on the FIRST column, so
	// the guard's early return is the exit under test.
	p.WithRowByteBudget(1, func(expr.Value) int64 { return 10 })
	p.WithRowBinder(binder)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out exec.Row
	if _, err := p.Next(&out); !errors.Is(err, exec.ErrProjectionRowTooLarge) {
		t.Fatalf("Next error = %v, want %v", err, exec.ErrProjectionRowTooLarge)
	}
	if binder.binds != 1 || binder.releases != 1 {
		t.Fatalf("binds=%d releases=%d after the byte budget tripped, want 1 of each",
			binder.binds, binder.releases)
	}
}

func TestProjectWithoutABinderBracketsNothing(t *testing.T) {
	// The un-fused build must be byte-identical: no binder, no bracket, and every
	// item still evaluated. A nil binder is the default, so this pins that the
	// bracket is genuinely conditional rather than calling into a zero value.
	binder := &recordingBinder{}
	items, seen := boundProbeItems(2, binder)
	p, err := exec.NewProject(newSliceOperator(exec.Row{expr.IntegerValue(1)}), items)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out exec.Row
	if ok, err := p.Next(&out); err != nil || !ok {
		t.Fatalf("Next = (%v, %v), want (true, nil)", ok, err)
	}
	if binder.binds != 0 || binder.releases != 0 {
		t.Fatalf("binds=%d releases=%d with no binder installed, want 0 of each",
			binder.binds, binder.releases)
	}
	if len(*seen) != 2 {
		t.Fatalf("%d item evaluations, want 2", len(*seen))
	}
	for i, wasBound := range *seen {
		if wasBound {
			t.Fatalf("item %d saw a bound row with no binder installed", i)
		}
	}
}

// TestColumnarProjectRowInputArmBracketsEachRow covers the second driver. Its
// fillers here always take the row-at-a-time fallback (the [exec.ProjectionItem]
// eval closure), which is the arm a real filler reaches for any cell it cannot read
// unboxed — and the arm that therefore needs the bracket.
//
// fillChunkFromChunk is deliberately NOT bracketed and is not exercised here: it is
// column-major and evaluates no row context.
func TestColumnarProjectRowInputArmBracketsEachRow(t *testing.T) {
	const rows, cols = 4, 2
	binder := &recordingBinder{}
	items, seen := boundProbeItems(cols, binder)
	fillers := make([]exec.ColumnFiller, cols)
	for i := range fillers {
		eval := items[i].Eval
		fillers[i] = func(row exec.Row, dst *exec.Chunk, col int) error {
			v, err := eval(row)
			if err != nil {
				return err
			}
			dst.PutValue(col, v)
			return nil
		}
	}

	src := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
		exec.Row{expr.IntegerValue(4)},
	)
	cp, err := exec.NewColumnarProject(src, items, fillers)
	if err != nil {
		t.Fatalf("NewColumnarProject: %v", err)
	}
	cp.WithRowBinder(binder)
	if err := cp.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cp.NewOutputChunk(rows)
	n, err := cp.FillChunk(dst, rows)
	if err != nil {
		t.Fatalf("FillChunk: %v", err)
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if n != rows {
		t.Fatalf("FillChunk appended %d rows, want %d", n, rows)
	}
	if binder.binds != rows || binder.releases != rows {
		t.Fatalf("binds=%d releases=%d over %d chunk rows, want %d of each: the row-input arm of "+
			"FillChunk owes the same bracket Project.Next does",
			binder.binds, binder.releases, rows, rows)
	}
	if binder.maxDepth != 1 {
		t.Fatalf("bracket nesting depth reached %d in FillChunk", binder.maxDepth)
	}
	if len(*seen) != rows*cols {
		t.Fatalf("%d filler evaluations, want %d", len(*seen), rows*cols)
	}
	for i, wasBound := range *seen {
		if !wasBound {
			t.Fatalf("filler fallback %d ran with no row bound", i)
		}
	}
}

// TestColumnarProjectRowInputArmReleasesOnFillerError pins the error exit of the
// second driver, for the same reason as the first: an unclosed bracket permanently
// strands a pooled binding map.
func TestColumnarProjectRowInputArmReleasesOnFillerError(t *testing.T) {
	binder := &recordingBinder{}
	boom := errors.New("filler exploded")
	items := []exec.ProjectionItem{
		{Alias: "a", Eval: func(_ exec.Row) (expr.Value, error) { return expr.IntegerValue(1), nil }},
		{Alias: "b", Eval: func(_ exec.Row) (expr.Value, error) { return expr.IntegerValue(2), nil }},
	}
	fillers := []exec.ColumnFiller{
		func(_ exec.Row, dst *exec.Chunk, col int) error {
			dst.PutValue(col, expr.IntegerValue(1))
			return nil
		},
		func(_ exec.Row, _ *exec.Chunk, _ int) error { return boom },
	}
	cp, err := exec.NewColumnarProject(newSliceOperator(exec.Row{expr.IntegerValue(1)}), items, fillers)
	if err != nil {
		t.Fatalf("NewColumnarProject: %v", err)
	}
	cp.WithRowBinder(binder)
	if err := cp.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cp.NewOutputChunk(4)
	if _, err := cp.FillChunk(dst, 4); !errors.Is(err, boom) {
		t.Fatalf("FillChunk error = %v, want %v", err, boom)
	}
	if binder.binds != 1 || binder.releases != 1 {
		t.Fatalf("binds=%d releases=%d after a failed filler, want 1 of each",
			binder.binds, binder.releases)
	}
}
