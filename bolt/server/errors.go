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
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
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

	// A writing or DDL statement issued inside a read-only transaction (BEGIN
	// with mode="r"). The request is invalid for the transaction's declared
	// access mode — a deterministic client fault — so it maps to Neo4j's
	// official request-invalid code. isClientFaultErr (derived from this
	// function) then forwards the sentinel's own message, which names only the
	// access-mode violation and discloses nothing internal.
	if errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
		return "Neo.ClientError.Request.Invalid"
	}

	// A write transaction that exceeds the store's per-transaction op cap
	// ([txn.ErrTransactionTooLarge], wrapped as "cypher: commit WAL: %w"). The
	// cap is deterministic — retrying the same transaction fails again — so
	// this is a ClientError (split the transaction), not a TransientError. The
	// code is Neo4j's official per-transaction resource-budget code.
	if errors.Is(err, txn.ErrTransactionTooLarge) {
		return "Neo.ClientError.General.TransactionOutOfMemoryError"
	}

	// A DURABILITY failure ([wal.ErrDurabilityFailed], rmp #2306): the write-ahead
	// log could not be made durable, the un-synced suffix was discarded, and the
	// writer is poisoned. It is the exact OPPOSITE of the conflict below and must not
	// be confused with it: retrying cannot help, because the handle is dead and the
	// next attempt fails the same way.
	//
	// Tested BEFORE the conflict so a poisoned commit can never be misread as
	// retriable if the two ever appear wrapped together.
	//
	// # Why DatabaseError and not TransientError
	//
	// The classification is what the driver's retry decision turns on:
	// neo4j-go-driver's IsRetriableTransient tests `classification ==
	// "TransientError"` (github.com/neo4j/neo4j-go-driver/v5 v5.28.4,
	// neo4j/db/errors.go), so a DatabaseError is never retried — which is the required
	// behaviour here. The code stays the generic General.UnknownError rather than a
	// more descriptive one because a better candidate would have to be verified
	// against Neo4j's own Status.java taxonomy, and this task did not read it; naming
	// an unverified code would be worse than a generic one that classifies correctly.
	// TestFailureCode_DurabilityFailureIsNotRetriedByTheRealDriver asks the driver
	// itself rather than asserting the classification.
	if errors.Is(err, wal.ErrDurabilityFailed) {
		return "Neo.DatabaseError.General.UnknownError"
	}

	// An MVCC write-write conflict ([mvcc.ErrSerializationConflict], rmp #2300):
	// the transaction tried to modify an object whose newest version it cannot
	// see. Unlike the cap above this IS worth retrying — a fresh snapshot
	// includes the change it collided with — so it must reach the driver as a
	// code the driver's own retry classifier accepts.
	//
	// # Why this exact code, verified rather than chosen
	//
	// SEMANTICS. neo4j/neo4j (branch master, read 2026-08-03;
	// community/common/src/main/java/org/neo4j/kernel/api/exceptions/Status.java,
	// enum Transaction) defines Outdated as TransientError with the description
	// "transaction has seen state which has been invalidated by applied
	// updates". That is this rule exactly: the writer's snapshot no longer
	// covers the chain head. DeadlockDetected was rejected — GoGraph never
	// waits, which is the documented reason PostgreSQL's wait-and-re-evaluate
	// shape was not taken (graph/mvcc/conflict.go).
	//
	// RETRIABILITY. github.com/neo4j/neo4j-go-driver/v5 v5.28.4 — the version in
	// go.mod — classifies in neo4j/db/errors.go: IsRetriable calls
	// IsRetriableTransient, which is `classification == "TransientError"`, the
	// second of the code's four dot-separated parts.
	//
	// THE TRAP. That same file's reclassify() runs BEFORE the classification is
	// parsed and rewrites TWO codes out of the family:
	//
	//	Neo.TransientError.Transaction.LockClientStopped -> Neo.ClientError...
	//	Neo.TransientError.Transaction.Terminated        -> Neo.ClientError...
	//
	// So `Neo.TransientError.Transaction.Terminated` — a plausible-looking
	// choice — is silently demoted to a ClientError and is NEVER retried.
	// Outdated is not on that list. TestFailureCode_SerializationConflictIsRetriedByTheRealDriver
	// asks the driver itself, both for this code and for the demoted one, so a
	// future edit to a "nearby" code cannot pass unnoticed.
	if errors.Is(err, mvcc.ErrSerializationConflict) {
		return "Neo.TransientError.Transaction.Outdated"
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

	// DROP CONSTRAINT naming a constraint that does not exist (without IF
	// EXISTS) — a deterministic client fault, mapped to Neo4j's official
	// constraint-drop-failed code rather than a generic database error.
	if errors.Is(err, exec.ErrConstraintNotFound) {
		return "Neo.ClientError.Schema.ConstraintDropFailed"
	}

	// CREATE CONSTRAINT naming a constraint whose (kind, label, property) is
	// already registered (without IF NOT EXISTS), or requesting a name already
	// held by a different constraint. Neo4j surfaces these as distinct
	// constraint-schema faults rather than a generic database error.
	if errors.Is(err, exec.ErrConstraintAlreadyExists) {
		return "Neo.ClientError.Schema.ConstraintAlreadyExists"
	}
	if errors.Is(err, exec.ErrConstraintNameConflict) {
		return "Neo.ClientError.Schema.ConstraintWithNameAlreadyExists"
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
	case strings.HasPrefix(ee.Msg, "ArgumentError:"):
		return "Neo.ClientError.Statement.ArgumentError", true
	// Runtime arithmetic faults are deterministic client conditions, not server
	// faults: integer divide/modulo by zero ("ArithmeticError:") and overflow
	// ("ArithmeticOverflow:") both map to ArithmeticError so the real message is
	// forwarded rather than replaced with generic internal-error text (#1765, #1766).
	case strings.HasPrefix(ee.Msg, "ArithmeticError:"),
		strings.HasPrefix(ee.Msg, "ArithmeticOverflow:"):
		return "Neo.ClientError.Statement.ArithmeticError", true
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
