package server

import (
	"context"
	"errors"
	"strings"

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

	// Cypher parse errors. Both types are produced by [parser.Parse] and reach
	// the Bolt layer wrapped as "cypher: parse: %w" (cypher/api.go
	// parseAndAnalyse), so errors.As is required to recover them.
	var pe *parser.ParseError
	if errors.As(err, &pe) {
		return "Neo.ClientError.Statement.SyntaxError"
	}
	var se *parser.SemaError
	if errors.As(err, &se) {
		return "Neo.ClientError.Statement.SemanticError"
	}

	// Scope-analysis errors: the engine returns [*sema.SemanticError] raw from
	// Run/RunInTx for a parseable-but-invalid query (e.g. an undefined
	// variable). Its Category field carries the TCK-pinned class — "SyntaxError"
	// or "TypeError" — which maps one-to-one onto the official Neo4j statement
	// codes; anything else falls back to the generic semantic-error code
	// (task #1353).
	var sse *sema.SemanticError
	if errors.As(err, &sse) {
		switch sse.Category {
		case sema.CategorySyntaxError:
			return "Neo.ClientError.Statement.SyntaxError"
		case sema.CategoryTypeError:
			return "Neo.ClientError.Statement.TypeError"
		default:
			return "Neo.ClientError.Statement.SemanticError"
		}
	}

	// Runtime evaluation errors: [*expr.EvalError] reaches the Bolt layer
	// wrapped by the executor (e.g. "exec: Project item %q eval: %w"). Only the
	// user-condition families are classified; evaluator-internal failures
	// (unsupported expression kinds, missing registries) keep the
	// UnknownError fallback.
	var ee *expr.EvalError
	if errors.As(err, &ee) {
		if code, ok := evalErrorCode(ee); ok {
			return code
		}
	}

	// An unsupported parameter type ([cypher.ErrUnsupportedParamType], wrapped
	// by [cypher.BindParams] as "cypher: BindParams: key %q: …"). The request
	// carried a value the engine cannot bind — a CLIENT fault — so it maps to
	// the official Neo4j type-error code rather than falling through to the
	// server-fault UnknownError. isClientFaultErr (derived from this function)
	// then forwards the message, which names only the offending Go type and
	// discloses nothing internal (task #1435).
	if errors.Is(err, cypher.ErrUnsupportedParamType) {
		return "Neo.ClientError.Statement.TypeError"
	}

	// A write transaction that exceeds the store's per-transaction op cap
	// ([txn.ErrTransactionTooLarge], wrapped as "cypher: commit WAL: %w"). The
	// cap is deterministic — retrying the same transaction fails again — so
	// this is a ClientError (split the transaction), not a TransientError. The
	// code is Neo4j's official per-transaction resource-budget code.
	if errors.Is(err, txn.ErrTransactionTooLarge) {
		return "Neo.ClientError.General.TransactionOutOfMemoryError"
	}

	// Resource-limit guards — a query whose result set or buffering aggregator
	// would exceed the engine's configured cap. These are client-controllable
	// conditions (a narrower query stays within budget), so they map to the same
	// LimitExceeded code the per-connection in-flight cursor cap uses, rather
	// than to a generic database error.
	if isResourceLimitErr(err) {
		return "Neo.ClientError.General.LimitExceeded"
	}

	// Constraint violations — both UNIQUE and NOT_NULL map to the official
	// Neo4j constraint-validation code (task #1353; previously the
	// non-taxonomy "ConstraintViolationOnCreate").
	var cv *exec.ConstraintViolationError
	if errors.As(err, &cv) {
		return "Neo.ClientError.Schema.ConstraintValidationFailed"
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

	// Plain (untyped) engine errors that carry a TCK category in their message
	// — e.g. "cypher: SyntaxError.NegativeIntegerArgument: …",
	// "cypher: SemanticError.MergeReadOwnWrites: …",
	// "cypher: ArgumentError.NumberOutOfRange: …" (cypher/api.go builds these
	// with fmt.Errorf, so no type to match). strings.Contains, not HasPrefix,
	// because some are re-wrapped (e.g. under "cypher: build plan: "). The
	// shapes are TCK-pinned, so matching on them is stable (task #1353).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "cypher: SyntaxError."):
		return "Neo.ClientError.Statement.SyntaxError"
	case strings.Contains(msg, "cypher: SemanticError."):
		return "Neo.ClientError.Statement.SemanticError"
	case strings.Contains(msg, "cypher: TypeError."):
		return "Neo.ClientError.Statement.TypeError"
	case strings.Contains(msg, "cypher: ArgumentError."):
		return "Neo.ClientError.Statement.ArgumentError"
	}

	return "Neo.DatabaseError.General.UnknownError"
}

// evalErrorCode classifies a runtime [*expr.EvalError] into a Neo4j status
// code. The evaluator exposes no structured kind, only a message whose
// leading token is the TCK-pinned error detail (e.g. "InvalidArgumentType:",
// "EntityNotFound:"), so the match is on those real, test-pinned prefixes.
// ok is false for evaluator messages that do not describe a user condition;
// the caller keeps its internal-error fallback for them.
func evalErrorCode(ee *expr.EvalError) (code string, ok bool) {
	switch {
	case strings.HasPrefix(ee.Msg, "InvalidArgumentType:"),
		strings.HasPrefix(ee.Msg, "MapElementAccessByNonString:"),
		strings.HasPrefix(ee.Msg, "incompatible types for comparison"):
		return "Neo.ClientError.Statement.TypeError", true
	case strings.HasPrefix(ee.Msg, "EntityNotFound:"):
		return "Neo.ClientError.Statement.EntityNotFound", true
	}
	return "", false
}

// isClientFaultErr reports whether err describes a condition caused by the
// client's own request — a syntax/semantic/type error, a constraint
// violation, a resource cap, an index/procedure misuse — whose message is the
// client's own diagnostic rather than internal server state. It derives from
// [FailureCode] so the status-code classification and [Session.sanitiseErr]'s
// message-forwarding decision can never diverge: every Neo.ClientError.* code
// except the Security family is client-fault. Security errors are excluded
// because the genuine cause of an authentication failure must not be
// disclosed to an unauthenticated peer.
func isClientFaultErr(err error) bool {
	code := FailureCode(err)
	return strings.HasPrefix(code, "Neo.ClientError.") &&
		!strings.HasPrefix(code, "Neo.ClientError.Security.")
}

// isResourceLimitErr reports whether err is one of the engine's bounded-resource
// guards: the per-query result-row cap ([cypher.ErrResultRowsExceeded]), the
// per-query aggregate-byte budget ([cypher.ErrResultBytesExceeded]), or a
// buffering aggregator's per-group element budget ([funcs.ErrCollectItemsExceeded]).
// All are tripped inside the graph's visibility barrier during materialisation,
// before any surplus rows reach the Bolt stream, so the server rejects the query
// cleanly rather than letting it exhaust memory.
//
// It is the single source of truth for classifying these errors: [FailureCode]
// uses it to pick the LimitExceeded code, which in turn makes
// [Session.sanitiseErr] (via [isClientFaultErr]) forward the cap's own message
// to the client verbatim (the messages name the limit and disclose nothing
// sensitive) instead of replacing it with the generic internal-error text.
func isResourceLimitErr(err error) bool {
	return errors.Is(err, cypher.ErrResultRowsExceeded) ||
		errors.Is(err, cypher.ErrResultBytesExceeded) ||
		errors.Is(err, funcs.ErrCollectItemsExceeded)
}
