package server

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/parser"
	"gograph/cypher/procs"
	"gograph/graph/index"
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
