package exec

// optional_expand.go — OptionalExpand operator (task-260).
//
// OptionalExpand wraps a regular Expand operator and adds NULL-extension
// semantics: if an input node has zero qualifying edges, a single output row is
// emitted with the edge and destination columns set to [expr.Null].
//
// This corresponds to OPTIONAL MATCH single-hop expansion in Cypher: a node
// that does not match the relationship pattern still produces one row (with
// NULLs), rather than being silently dropped.
//
// # Schema
//
// Output row = input row || [srcID, edgeID, dstID].
// When zero edges are found for an input node: srcID = input node ID,
// edgeID = Null, dstID = Null.
// When one or more edges are found, the output is identical to Expand's output.
//
// # Implementation strategy
//
// OptionalExpand drives the inner Expand per-input-row by re-initialising it
// with a sliceOperator of one row. If no rows emerge from Expand, it emits the
// NULL-extended row itself.
//
// # Concurrency
//
// OptionalExpand is NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call.

import (
	"context"

	"gograph/cypher/expr"
)

// OptionalExpand is a Volcano pipeline operator that performs a single-hop
// expansion and emits a NULL-extended row when no edges match for an input node.
//
// OptionalExpand is NOT safe for concurrent use.
type OptionalExpand struct {
	child     Operator   // the wrapped Expand operator
	singleArg *singleRow // injects one row at a time into child
	input     Operator   // original input operator (upstream of OptionalExpand)
	inputCol  int        // column holding the source NodeID in each input row

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// per-outer-row state
	inputRow     Row  // current input row
	pendingInput bool // true when inputRow is loaded but child has not been drained
	emittedAny   bool // true when at least one row has been emitted from child for this input
	childEOS     bool // true when outer input is exhausted

	outBuf []expr.Value
}

// singleRow is a minimal operator that emits exactly one pre-loaded row.
// It is used to feed individual input rows into the child Expand one at a time.
type singleRow struct {
	row     Row
	emitted bool
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

func (s *singleRow) Init(ctx context.Context) error {
	s.ctx = ctx
	s.emitted = false
	return nil
}

func (s *singleRow) Next(out *Row) (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return false, err
	}
	if s.emitted {
		return false, nil
	}
	*out = s.row
	s.emitted = true
	return true, nil
}

func (s *singleRow) Close() error { return nil }

// NewOptionalExpand creates an OptionalExpand operator.
//   - input is the upstream operator supplying node IDs.
//   - fwd is the forward CSR adjacency.
//   - rev is the reverse CSR adjacency (required for DirIn/DirBoth).
//   - cfg is the Expand configuration (direction, edge-type filter, inputCol).
//
// The NULL-extension row uses the same column layout as Expand:
// inputRow... || srcID || Null(edgeID) || Null(dstID).
func NewOptionalExpand(input Operator, fwd, rev csrAdjacency, cfg ExpandConfig) *OptionalExpand {
	sr := &singleRow{}
	child := NewExpand(sr, fwd, rev, cfg)
	return &OptionalExpand{
		child:     child,
		singleArg: sr,
		input:     input,
		inputCol:  cfg.InputCol,
	}
}

// Init initialises the operator.
func (op *OptionalExpand) Init(ctx context.Context) error {
	op.ctx = ctx
	op.pendingInput = false
	op.childEOS = false
	op.emittedAny = false
	op.inputRow = nil
	op.outBuf = op.outBuf[:0]
	return op.input.Init(ctx)
}

// Next emits the next row. For each input row:
//   - It feeds the row into the inner Expand one hop at a time.
//   - If Expand emits ≥1 row, those rows are forwarded as-is.
//   - If Expand emits 0 rows for an input node, a NULL-extended row is emitted.
func (op *OptionalExpand) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// If we have a current input row being processed, pull from child Expand.
		if op.pendingInput {
			var childRow Row
			ok, err := op.child.Next(&childRow)
			if err != nil {
				return false, err
			}
			if ok {
				op.emittedAny = true
				// Copy child row into stable buffer.
				need := len(childRow)
				if cap(op.outBuf) < need {
					op.outBuf = make([]expr.Value, need)
				}
				op.outBuf = op.outBuf[:need]
				copy(op.outBuf, childRow)
				*out = op.outBuf
				return true, nil
			}
			// Child exhausted for this input row.
			op.pendingInput = false
			if !op.emittedAny {
				// Zero matches → emit NULL-extended row.
				row := op.buildNullRow(op.inputRow)
				*out = row
				return true, nil
			}
			// At least one match was emitted; continue to next input row.
			continue
		}

		if op.childEOS {
			return false, nil
		}

		// Pull the next input row.
		var inputRow Row
		ok, err := op.input.Next(&inputRow)
		if err != nil {
			return false, err
		}
		if !ok {
			op.childEOS = true
			return false, nil
		}

		// Stable snapshot.
		cp := make(Row, len(inputRow))
		copy(cp, inputRow)
		op.inputRow = cp

		// Feed this single row into the child Expand.
		op.singleArg.row = cp
		if err := op.child.Init(op.ctx); err != nil {
			return false, err
		}
		op.pendingInput = true
		op.emittedAny = false
	}
}

// buildNullRow constructs the NULL-extended output row:
// inputRow... || srcID || Null(edgeID) || Null(dstID).
func (op *OptionalExpand) buildNullRow(inputRow Row) Row {
	srcID := expr.Value(expr.Null)
	if op.inputCol >= 0 && op.inputCol < len(inputRow) {
		srcID = inputRow[op.inputCol]
	}
	need := len(inputRow) + 3
	buf := make([]expr.Value, need)
	copy(buf, inputRow)
	buf[len(inputRow)] = srcID
	buf[len(inputRow)+1] = expr.Null
	buf[len(inputRow)+2] = expr.Null
	return buf
}

// Close closes the input and child operators.
func (op *OptionalExpand) Close() error {
	inputErr := op.input.Close()
	childErr := op.child.Close()
	op.outBuf = nil
	op.inputRow = nil
	if inputErr != nil {
		return inputErr
	}
	return childErr
}
