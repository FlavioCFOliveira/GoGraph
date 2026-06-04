package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
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
			name: "ConstraintViolationError UNIQUE",
			err:  &exec.ConstraintViolationError{Kind: "UNIQUE", Label: "Person", Property: "email"},
			want: "Neo.ClientError.Schema.ConstraintViolationOnCreate",
		},
		{
			name: "ConstraintViolationError NOT_NULL",
			err:  &exec.ConstraintViolationError{Kind: "NOT_NULL", Label: "Person", Property: "name"},
			want: "Neo.ClientError.Schema.ConstraintViolationOnCreate",
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
