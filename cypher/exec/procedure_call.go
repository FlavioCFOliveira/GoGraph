package exec

// procedure_call.go — ProcedureCallOp Volcano operator (task-301).
//
// ProcedureCallOp implements the CALL clause by looking up and invoking a
// registered procedure for each driving row produced by its child. When no
// child is provided (standalone CALL at query start) a single synthetic empty
// row drives the procedure exactly once.
//
// # Execution model
//
// For each driving row the operator:
//  1. Evaluates its argExprs against the driver row.
//  2. Invokes the procedure via reg.Lookup + entry.Impl.
//  3. Buffers all result rows internally.
//  4. Emits buffered rows one at a time via successive Next calls.
//
// When the child is exhausted and all buffered rows have been emitted, Next
// returns (false, nil).
//
// # YIELD filtering
//
// Column subsetting (YIELD col1, col2 from a wider procedure) is delegated to
// a downstream Projection operator. ProcedureCallOp emits all columns returned
// by the procedure implementation.
//
// # Concurrency
//
// ProcedureCallOp is NOT safe for concurrent use.

import (
	"context"
	"fmt"

	"gograph/cypher/expr"
	"gograph/cypher/procs"
)

// ProcedureCallOp invokes a registered procedure and emits its result rows.
//
// ProcedureCallOp is NOT safe for concurrent use.
type ProcedureCallOp struct {
	namespace []string
	name      string
	argExprs  []func(Row) (expr.Value, error)
	yieldVars []string
	child     Operator // nil for standalone CALL
	reg       *procs.Registry

	ctx              context.Context //nolint:containedctx // stored for per-Next ctx check
	rows             [][]expr.Value
	rowIdx           int
	currentDriverRow Row  // driver row whose result is currently buffered
	doneChild        bool // true once the child (or synthetic single row) is exhausted
}

// NewProcedureCallOp creates a ProcedureCallOp.
//
// namespace and name identify the procedure. argExprs evaluate procedure
// arguments against the current driver row. yieldVars names the output columns.
// child is the driving subplan; pass nil for a standalone CALL. reg is the
// procedure registry used for lookup at runtime.
func NewProcedureCallOp(
	namespace []string,
	name string,
	argExprs []func(Row) (expr.Value, error),
	yieldVars []string,
	child Operator,
	reg *procs.Registry,
) *ProcedureCallOp {
	return &ProcedureCallOp{
		namespace: namespace,
		name:      name,
		argExprs:  argExprs,
		yieldVars: yieldVars,
		child:     child,
		reg:       reg,
	}
}

// Init resets internal state and initialises the child if present.
func (op *ProcedureCallOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.rows = nil
	op.rowIdx = 0
	op.doneChild = false
	if op.child != nil {
		return op.child.Init(ctx)
	}
	return nil
}

// Next advances the operator by one row.
//
// It draws driving rows from the child (or a synthetic empty row when child is
// nil), invokes the procedure for each, buffers all result rows, and emits
// them one at a time.
//
// Void procedure semantics. A procedure declared with no output columns
// (len(yieldVars) == 0) is treated as a side-effect-only step. When
// invoked in-query (op.child != nil) it must NOT consume the driver row;
// instead each driver row is emitted unchanged once the impl has run,
// preserving the upstream variable bindings for any downstream RETURN.
// Standalone CALL (op.child == nil) emits nothing.
func (op *ProcedureCallOp) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// Emit next buffered row when available. The driver row is
		// prefixed onto each result row so downstream operators (RETURN,
		// further CALL/MATCH) still see upstream bindings — `CALL …
		// YIELD label WITH count(*) AS c CALL … YIELD label` needs `c`
		// to flow through to RETURN (Call6 [1]).
		if op.rowIdx < len(op.rows) {
			combined := make(Row, 0, len(op.currentDriverRow)+len(op.rows[op.rowIdx]))
			combined = append(combined, op.currentDriverRow...)
			combined = append(combined, op.rows[op.rowIdx]...)
			*out = combined
			op.rowIdx++
			return true, nil
		}

		// All buffered rows consumed; fetch the next driver row.
		if op.doneChild {
			return false, nil
		}

		var driverRow Row
		if op.child != nil {
			var childOut Row
			ok, err := op.child.Next(&childOut)
			if err != nil {
				return false, err
			}
			if !ok {
				op.doneChild = true
				return false, nil
			}
			driverRow = childOut
		} else {
			// Standalone CALL: drive exactly once with an empty row.
			op.doneChild = true
			driverRow = Row{}
		}

		// Evaluate argument expressions.
		args, err := op.evalArgs(driverRow)
		if err != nil {
			return false, fmt.Errorf("exec: ProcedureCallOp %s: eval args: %w", op.fqn(), err)
		}

		// Look up and invoke the procedure.
		entry, err := op.reg.Lookup(op.namespace, op.name)
		if err != nil {
			return false, fmt.Errorf("exec: ProcedureCallOp: %w", err)
		}
		resultRows, err := entry.Impl(op.ctx, args)
		if err != nil {
			return false, fmt.Errorf("exec: ProcedureCallOp %s: %w", op.fqn(), err)
		}

		// Void-procedure passthrough. When the procedure declares no
		// outputs and there is a child, the driver row is emitted once
		// per call so downstream operators still see the upstream
		// bindings. Standalone CALL (no child) emits nothing.
		if len(op.yieldVars) == 0 && len(resultRows) == 0 && op.child != nil {
			*out = append((*out)[:0], driverRow...)
			return true, nil
		}

		// Buffer results and the driver row that produced them; loop back
		// to emit them.
		op.rows = resultRows
		op.rowIdx = 0
		op.currentDriverRow = driverRow
	}
}

// Close releases resources and closes the child operator.
func (op *ProcedureCallOp) Close() error {
	op.rows = nil
	if op.child != nil {
		return op.child.Close()
	}
	return nil
}

// evalArgs evaluates all argument expressions against driverRow.
func (op *ProcedureCallOp) evalArgs(driverRow Row) ([]expr.Value, error) {
	if len(op.argExprs) == 0 {
		return nil, nil
	}
	args := make([]expr.Value, len(op.argExprs))
	for i, fn := range op.argExprs {
		v, err := fn(driverRow)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return args, nil
}

// fqn returns the fully-qualified procedure name for error messages.
func (op *ProcedureCallOp) fqn() string {
	if len(op.namespace) == 0 {
		return op.name
	}
	result := ""
	for _, ns := range op.namespace {
		result += ns + "."
	}
	return result + op.name
}
