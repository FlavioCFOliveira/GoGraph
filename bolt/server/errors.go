package server

import (
	"context"
	"errors"

	"gograph/cypher/exec"
	"gograph/cypher/parser"
	"gograph/cypher/procs"
	"gograph/graph/index"
)

// FailureCode returns the Neo4j-style dot-delimited error code for err.
// Falls back to "Neo.DatabaseError.General.UnknownError" for unrecognised
// errors. The lookup uses errors.As and errors.Is so wrapped errors are
// matched correctly.
func FailureCode(err error) string {
	if err == nil {
		return "Neo.DatabaseError.General.UnknownError"
	}

	// Context errors first — they wrap common sentinel values.
	if errors.Is(err, context.DeadlineExceeded) {
		return "Neo.ClientError.Transaction.TransactionTimedOut"
	}
	if errors.Is(err, context.Canceled) {
		return "Neo.ClientError.Transaction.Terminated"
	}

	// Auth / session errors from this package.
	if errors.Is(err, ErrAuthFailed) {
		return "Neo.ClientError.Security.Unauthorized"
	}
	if errors.Is(err, ErrInvalidTransition) {
		return "Neo.ClientError.Request.InvalidFormat"
	}

	// Cypher parse errors.
	var pe *parser.ParseError
	if errors.As(err, &pe) {
		return "Neo.ClientError.Statement.SyntaxError"
	}
	var se *parser.SemaError
	if errors.As(err, &se) {
		return "Neo.ClientError.Statement.SemanticError"
	}

	// Constraint violations — both UNIQUE and NOT_NULL map to the same code per
	// the task specification.
	var cv *exec.ConstraintViolationError
	if errors.As(err, &cv) {
		return "Neo.ClientError.Schema.ConstraintViolationOnCreate"
	}

	// Index errors.
	if errors.Is(err, index.ErrIndexExists) {
		return "Neo.ClientError.Schema.IndexAlreadyExists"
	}
	if errors.Is(err, index.ErrIndexNotFound) {
		return "Neo.ClientError.Schema.IndexNotFound"
	}

	// Procedure errors.
	if errors.Is(err, procs.ErrProcNotFound) {
		return "Neo.ClientError.Procedure.ProcedureNotFound"
	}

	return "Neo.DatabaseError.General.UnknownError"
}
