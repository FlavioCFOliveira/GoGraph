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
	"fmt"

	"gograph/cypher/expr"
)

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
	child  Operator
	items  []ProjectionItem
	ctx    context.Context //nolint:containedctx // stored for per-Next ctx check
	outBuf Row             // reusable output backing slice; len = len(items)
}

// NewProject creates a Project operator.  items defines the output schema;
// each item's Eval function is applied to each input row.  items must not be
// empty.
func NewProject(child Operator, items []ProjectionItem) (*Project, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("exec: Project requires at least one projection item")
	}
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

	var inputRow Row
	ok, err := op.child.Next(&inputRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	for i, item := range op.items {
		v, err := item.Eval(inputRow)
		if err != nil {
			return false, fmt.Errorf("exec: Project item %q eval: %w", item.Alias, err)
		}
		op.outBuf[i] = v
	}

	*out = op.outBuf
	return true, nil
}

// Close releases resources and closes the child operator.
func (op *Project) Close() error {
	return op.child.Close()
}
