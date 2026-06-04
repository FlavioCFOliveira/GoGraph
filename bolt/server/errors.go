package server

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
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

	// Resource-limit guards — a query whose result set or buffering aggregator
	// would exceed the engine's configured cap. These are client-controllable
	// conditions (a narrower query stays within budget), so they map to the same
	// LimitExceeded code the per-connection in-flight cursor cap uses, rather
	// than to a generic database error.
	if isResourceLimitErr(err) {
		return "Neo.ClientError.General.LimitExceeded"
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

// isResourceLimitErr reports whether err is one of the engine's bounded-resource
// guards: the per-query result-row cap ([cypher.ErrResultRowsExceeded]) or a
// buffering aggregator's per-group element budget ([funcs.ErrCollectItemsExceeded]).
// Both are tripped inside the graph's visibility barrier during materialisation,
// before any surplus rows reach the Bolt stream, so the server rejects the query
// cleanly rather than letting it exhaust memory.
//
// It is the single source of truth for classifying these errors: [FailureCode]
// uses it to pick the LimitExceeded code, and [Session.sanitiseErr] uses it to
// forward the cap's own message to the client verbatim (the messages name the
// limit and disclose nothing sensitive) instead of replacing it with the generic
// internal-error text.
func isResourceLimitErr(err error) bool {
	return errors.Is(err, cypher.ErrResultRowsExceeded) ||
		errors.Is(err, funcs.ErrCollectItemsExceeded)
}
