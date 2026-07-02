package exec

// project.go — Project operator (RETURN / WITH projection) (task-243).
//
// Project evaluates a list of projection items against each input row and
// assembles an output row whose columns correspond to the evaluated results.
// This models the RETURN and WITH clauses in Cypher: each item may be an
// arbitrary expression and carries an alias that names the output column.
//
// # Output schema
//
// The output row has exactly len(items) columns, one per ProjectionItem, in
// declaration order.
//
// # Zero-alloc note
//
// The output row backing slice is allocated once during Init (sized to
// len(items)) and reused on every Next call. Callers that need to retain the
// row across multiple Next calls must copy it.
//
// # Concurrency
//
// Project is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrProjectionRowTooLarge is returned by [Project.Next] when the estimated
// size of a single assembled output row exceeds the configured per-row byte
// budget. It bounds the transient peak of one row's construction (e.g. a
// RETURN that projects several large list columns) to the ceiling plus one
// column, independent of the column count, so a query cannot OOM the process by
// compounding many big columns into one row (#1852).
var ErrProjectionRowTooLarge = errors.New("exec: projection row memory cap exceeded")

// ProjectionItem describes a single column in a projection.  Eval is evaluated
// against the input row; Alias names the resulting output column.
type ProjectionItem struct {
	// Alias is the output column name (e.g. "n", "count(n)", "x").
	Alias string
	// Eval evaluates the item expression against the current input row and
	// returns the projected value.
	Eval func(Row) (expr.Value, error)
}

// Project is a Volcano pipeline operator that applies a list of [ProjectionItem]
// expressions to each input row, producing an output row with one column per
// item.
//
// Project is NOT safe for concurrent use.
type Project struct {
	child    Operator
	items    []ProjectionItem
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check
	outBuf   Row             // reusable output backing slice; len = len(items)
	inputRow Row             // reusable scratch header for the child's row (see Next)
	// maxRowBytes and estimateValue bound the estimated size of a single
	// assembled output row (0 = disabled). See WithRowByteBudget.
	maxRowBytes   int64
	estimateValue func(expr.Value) int64
}

// WithRowByteBudget bounds the estimated size of a single assembled output row
// by maxRowBytes, using estimateValue for the per-column estimate. It is
// enforced INCREMENTALLY inside Next — after each column is evaluated, before
// the next — so a projection of several large columns (e.g. RETURN range(1,N),
// range(1,N), …) is rejected before the whole row is materialised, bounding the
// transient peak to maxRowBytes plus one column regardless of the column count.
// It complements the drain's aggregate per-result byte budget, which is a
// retention guard on the SUM of already-built rows and therefore fires only
// after Next has assembled the row; this per-row guard moves the same
// accounting earlier and makes it per-column so construction cannot OOM (#1852).
// A non-positive maxRowBytes or nil estimateValue leaves the guard disabled
// (behaviour-preserving). Returns op for chaining; call before Init.
func (op *Project) WithRowByteBudget(maxRowBytes int64, estimateValue func(expr.Value) int64) *Project {
	op.maxRowBytes = maxRowBytes
	op.estimateValue = estimateValue
	return op
}

// NewProject creates a Project operator.  items defines the output schema;
// each item's Eval function is applied to each input row.  An empty items
// slice is legal (e.g. `WITH *` over a pattern that binds no variables);
// the resulting operator forwards an empty Row for every input row.
func NewProject(child Operator, items []ProjectionItem) (*Project, error) {
	return &Project{
		child:  child,
		items:  items,
		outBuf: make(Row, len(items)),
	}, nil
}

// Columns returns the ordered list of output column aliases.
func (op *Project) Columns() []string {
	cols := make([]string, len(op.items))
	for i, item := range op.items {
		cols[i] = item.Alias
	}
	return cols
}

// Init initialises the operator and its child.
func (op *Project) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next evaluates each projection item against the next input row and writes
// the result into out.  Returns (true, nil) when a projected row is available,
// (false, nil) at end-of-stream, or (false, err) on evaluation or child error.
func (op *Project) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	// Reuse op.inputRow across calls so &op.inputRow points into the already
	// heap-allocated Project struct rather than forcing a fresh per-row scratch
	// header onto the heap (the child's Next takes *Row by pointer through the
	// Operator interface, which defeats escape analysis on a local). The child
	// writes a slice it owns into op.inputRow; we only read it within this call
	// and never retain it past the loop, so sharing one header is sound.
	ok, err := op.child.Next(&op.inputRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	var rowBytes int64
	for i, item := range op.items {
		v, err := item.Eval(op.inputRow)
		if err != nil {
			return false, fmt.Errorf("exec: Project item %q eval: %w", item.Alias, err)
		}
		op.outBuf[i] = v
		// Charge the just-built column against the per-row byte budget
		// incrementally — before evaluating the next column — so a row that
		// compounds several large columns is rejected before it is fully
		// materialised (bounding the transient peak to the ceiling plus one
		// column). Disabled when no budget is configured. See WithRowByteBudget.
		if op.maxRowBytes > 0 && op.estimateValue != nil {
			rowBytes += op.estimateValue(v)
			if rowBytes > op.maxRowBytes {
				return false, ErrProjectionRowTooLarge
			}
		}
	}

	*out = op.outBuf
	return true, nil
}

// Close releases resources and closes the child operator.
func (op *Project) Close() error {
	return op.child.Close()
}

// rowCountHint forwards the child's upper-bound row count unchanged. Project is
// a strict 1:1 pass-through — exactly one output row per input row, never
// dropped, multiplied, or collapsed — so the child's bound is the operator's
// bound. It satisfies [rowCountHinter] so a presize hint propagates from a leaf
// scan through the final projection that BuildPlan wraps every plan in (#1720).
// If the child exposes no hint, neither does Project.
func (op *Project) rowCountHint() (int, bool) {
	if h, ok := op.child.(rowCountHinter); ok {
		return h.rowCountHint()
	}
	return 0, false
}
