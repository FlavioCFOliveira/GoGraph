package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 313: FailureCode mapping tests (15 cases, one per error type)
// ─────────────────────────────────────────────────────────────────────────────

func TestFailureCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ParseError",
			err:  &parser.ParseError{Message: "unexpected token"},
			want: "Neo.ClientError.Statement.SyntaxError",
		},
		{
			name: "ParseError wrapped",
			err:  fmt.Errorf("outer: %w", &parser.ParseError{Message: "bad syntax"}),
			want: "Neo.ClientError.Statement.SyntaxError",
		},
		{
			name: "SemaError",
			err:  &parser.SemaError{Rule: "match", Message: "unsupported"},
			want: "Neo.ClientError.Statement.SemanticError",
		},
		{
			name: "SemaError wrapped",
			err:  fmt.Errorf("wrap: %w", &parser.SemaError{Rule: "r", Message: "m"}),
			want: "Neo.ClientError.Statement.SemanticError",
		},
		{
			// task #1353: the engine returns *sema.SemanticError (not
			// *parser.SemaError) for scope violations; the Category field is
			// TCK-pinned and selects the statement code.
			name: "sema.SemanticError SyntaxError category",
			err:  &sema.SemanticError{Category: sema.CategorySyntaxError, SubType: "UndefinedVariable"},
			want: "Neo.ClientError.Statement.SyntaxError",
		},
		{
			name: "sema.SemanticError TypeError category",
			err:  &sema.SemanticError{Category: sema.CategoryTypeError, SubType: "InvalidArgumentType"},
			want: "Neo.ClientError.Statement.TypeError",
		},
		{
			name: "sema.SemanticError unknown category",
			err:  &sema.SemanticError{Category: "SomethingElse", SubType: "X"},
			want: "Neo.ClientError.Statement.SemanticError",
		},
		{
			// task #1353: runtime evaluation type errors, wrapped by the
			// executor exactly as exec.Project does.
			name: "expr.EvalError InvalidArgumentType wrapped",
			err:  fmt.Errorf("exec: Project item %q eval: %w", "l[1.5]", &expr.EvalError{Msg: "InvalidArgumentType: list index must be Integer, got Float"}),
			want: "Neo.ClientError.Statement.TypeError",
		},
		{
			name: "expr.EvalError MapElementAccessByNonString",
			err:  &expr.EvalError{Msg: "MapElementAccessByNonString: map key must be String, got Integer"},
			want: "Neo.ClientError.Statement.TypeError",
		},
		{
			name: "expr.EvalError EntityNotFound",
			err:  &expr.EvalError{Msg: "EntityNotFound: DeletedEntityAccess: cannot read labels of deleted node"},
			want: "Neo.ClientError.Statement.EntityNotFound",
		},
		{
			// Evaluator-internal failures stay on the internal-error fallback.
			name: "expr.EvalError internal",
			err:  &expr.EvalError{Msg: "unsupported expression type *ast.Foo"},
			want: "Neo.DatabaseError.General.UnknownError",
		},
		{
			// task #1353: per-transaction op cap, wrapped exactly as the
			// engine's commit path does.
			name: "txn.ErrTransactionTooLarge wrapped",
			err:  fmt.Errorf("cypher: commit WAL: %w", txn.ErrTransactionTooLarge),
			want: "Neo.ClientError.General.TransactionOutOfMemoryError",
		},
		{
			// task #1353: untyped engine errors carrying a TCK category in the
			// message (built with fmt.Errorf in cypher/api.go).
			name: "plain SemanticError message shape",
			err:  fmt.Errorf("cypher: SemanticError.MergeReadOwnWrites: MERGE pattern contains a null property literal"),
			want: "Neo.ClientError.Statement.SemanticError",
		},
		{
			name: "plain ArgumentError message shape",
			err:  fmt.Errorf("cypher: ArgumentError.NumberOutOfRange: percentile argument of 2.0 must be between 0.0 and 1.0"),
			want: "Neo.ClientError.Statement.ArgumentError",
		},
		{
			name: "plain SyntaxError message shape rewrapped",
			err:  fmt.Errorf("cypher: build plan: %w", fmt.Errorf("cypher: SyntaxError.NegativeIntegerArgument: LIMIT requires a non-negative integer, got -1")),
			want: "Neo.ClientError.Statement.SyntaxError",
		},
		{
			// task #1353: constraint violations map to the official Neo4j
			// taxonomy code (previously the non-taxonomy
			// "ConstraintViolationOnCreate").
			name: "ConstraintViolationError UNIQUE",
			err:  &exec.ConstraintViolationError{Kind: "UNIQUE", Label: "Person", Property: "email"},
			want: "Neo.ClientError.Schema.ConstraintValidationFailed",
		},
		{
			name: "ConstraintViolationError NOT_NULL",
			err:  &exec.ConstraintViolationError{Kind: "NOT_NULL", Label: "Person", Property: "name"},
			want: "Neo.ClientError.Schema.ConstraintValidationFailed",
		},
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: "Neo.ClientError.Transaction.TransactionTimedOut",
		},
		{
			name: "context.DeadlineExceeded wrapped",
			err:  fmt.Errorf("timed out: %w", context.DeadlineExceeded),
			want: "Neo.ClientError.Transaction.TransactionTimedOut",
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: "Neo.ClientError.Transaction.Terminated",
		},
		{
			name: "index.ErrIndexExists",
			err:  fmt.Errorf("create: %w", index.ErrIndexExists),
			want: "Neo.ClientError.Schema.IndexAlreadyExists",
		},
		{
			name: "index.ErrIndexNotFound",
			err:  fmt.Errorf("drop: %w", index.ErrIndexNotFound),
			want: "Neo.ClientError.Schema.IndexNotFound",
		},
		{
			name: "procs.ErrProcNotFound",
			err:  fmt.Errorf("call: %w", procs.ErrProcNotFound),
			want: "Neo.ClientError.Procedure.ProcedureNotFound",
		},
		{
			// task #1293: the engine's result-row cap maps to the resource-limit
			// code, matching the per-connection in-flight cursor cap.
			name: "cypher.ErrResultRowsExceeded",
			err:  cypher.ErrResultRowsExceeded,
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			name: "cypher.ErrResultRowsExceeded wrapped",
			err:  fmt.Errorf("drain: %w", cypher.ErrResultRowsExceeded),
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			// task #1328: the engine's aggregate-byte budget maps to the same
			// resource-limit code as the row cap; it reaches the Bolt layer via
			// Result.Err() (tripped inside the visibility barrier on materialise).
			name: "cypher.ErrResultBytesExceeded",
			err:  cypher.ErrResultBytesExceeded,
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			name: "cypher.ErrResultBytesExceeded wrapped",
			err:  fmt.Errorf("drain: %w", cypher.ErrResultBytesExceeded),
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			// task #1294: the buffering-aggregator element budget maps to the same
			// resource-limit code; it reaches the Bolt layer via Result.Err().
			name: "funcs.ErrCollectItemsExceeded",
			err:  funcs.ErrCollectItemsExceeded,
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			name: "funcs.ErrCollectItemsExceeded wrapped",
			err:  fmt.Errorf("aggregate: %w", funcs.ErrCollectItemsExceeded),
			want: "Neo.ClientError.General.LimitExceeded",
		},
		{
			name: "ErrAuthFailed",
			err:  ErrAuthFailed,
			want: "Neo.ClientError.Security.Unauthorized",
		},
		{
			name: "ErrInvalidTransition",
			err:  ErrInvalidTransition,
			want: "Neo.ClientError.Request.InvalidFormat",
		},
		{
			name: "ErrWriteInReadOnlyTx",
			err:  cypher.ErrWriteInReadOnlyTx,
			want: "Neo.ClientError.Request.Invalid",
		},
		{
			name: "ErrWriteInReadOnlyTx wrapped",
			err:  fmt.Errorf("bolt: run: %w", cypher.ErrWriteInReadOnlyTx),
			want: "Neo.ClientError.Request.Invalid",
		},
		{
			name: "unknown error",
			err:  fmt.Errorf("something went wrong"),
			want: "Neo.DatabaseError.General.UnknownError",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FailureCode(tc.err)
			if got != tc.want {
				t.Errorf("FailureCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
