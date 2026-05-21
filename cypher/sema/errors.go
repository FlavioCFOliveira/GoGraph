// Package sema implements the scope-analysis pass for openCypher queries.
// It operates on a parsed [gograph/cypher/ast.Query] and enforces variable
// scoping rules: WITH boundaries, UNWIND introduction, undefined references,
// and redeclaration within the same scope.
//
// Concurrency: [Analyse] is a pure function; the returned slice of errors is
// safe for concurrent reads after the call returns. Input AST nodes are treated
// as immutable (see [gograph/cypher/ast] package documentation).
package sema

import (
	"fmt"

	"gograph/cypher/ast"
)

// ErrorKind classifies a scope-analysis violation.
type ErrorKind string

const (
	// KindUndefinedVar is reported when an expression references a variable
	// that has not been introduced by any preceding clause in the current scope.
	KindUndefinedVar ErrorKind = "UNDEFINED_VAR"

	// KindRedeclaration is reported when a variable is introduced a second time
	// within the same scope without a WITH boundary that would shadow it.
	KindRedeclaration ErrorKind = "REDECLARATION"

	// KindScopeLeak is reported when a variable introduced inside a sub-scope
	// (e.g. a list comprehension) is referenced outside that scope.
	KindScopeLeak ErrorKind = "SCOPE_LEAK"
)

// ScopeError is the error type produced by the scope-analysis pass.
// It implements the standard error interface.
type ScopeError struct {
	// Kind classifies the violation; one of the Kind* constants.
	Kind ErrorKind
	// Pos is the source position of the offending token or node.
	Pos ast.Position
	// Message is a human-readable description.
	Message string
}

// Error implements the error interface.
func (e *ScopeError) Error() string {
	return fmt.Sprintf("scope error at %s [%s]: %s", e.Pos, e.Kind, e.Message)
}

// undefinedVarError constructs a KindUndefinedVar ScopeError.
func undefinedVarError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindUndefinedVar,
		Pos:     pos,
		Message: fmt.Sprintf("undefined variable %q", name),
	}
}

// redeclarationError constructs a KindRedeclaration ScopeError.
func redeclarationError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindRedeclaration,
		Pos:     pos,
		Message: fmt.Sprintf("variable %q already declared in this scope", name),
	}
}

// ScopeLeakError constructs a KindScopeLeak ScopeError, returned when a
// variable introduced inside a sub-scope is referenced outside that scope.
// It is exported so that callers and future analysis passes can build
// KindScopeLeak errors with a consistent message format.
func ScopeLeakError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindScopeLeak,
		Pos:     pos,
		Message: fmt.Sprintf("variable %q is not visible outside its declaring scope", name),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Bolt-compatible mapping (TCK error categories)
// ─────────────────────────────────────────────────────────────────────────────

// Bolt-compatible error category / sub-type strings raised at compile time
// by the semantic-analysis pass. They mirror the openCypher TCK expectations:
//
//	"a <Category> should be raised at compile time: <SubType>"
//
// See cypher/tck/features/**/*.feature for the full enumeration. Only the
// subset emitted by [Analyse] is defined here.
const (
	// CategorySyntaxError matches the TCK "a SyntaxError should be raised"
	// step. Used for scope violations such as UndefinedVariable and
	// VariableTypeConflict.
	CategorySyntaxError = "SyntaxError"

	// CategoryTypeError matches the TCK "a TypeError should be raised" step.
	// Reserved for static type mismatches surfaced by future passes.
	CategoryTypeError = "TypeError"

	// SubTypeUndefinedVariable is the canonical TCK sub-type for references
	// to variables that are not in scope. Produced from KindUndefinedVar and
	// KindScopeLeak (both surface as "variable not visible here").
	SubTypeUndefinedVariable = "UndefinedVariable"

	// SubTypeVariableTypeConflict is the TCK sub-type for re-introductions
	// of a name with an incompatible type within the same scope. Produced
	// from KindRedeclaration.
	SubTypeVariableTypeConflict = "VariableTypeConflict"

	// SubTypeInvalidArgumentType is the TCK sub-type for operator/function
	// argument type mismatches detected at compile time. Reserved for
	// [TypeError] use.
	SubTypeInvalidArgumentType = "InvalidArgumentType"
)

// SemanticError is the engine-facing wrapper around one or more
// [ScopeError]s. It carries the Bolt-compatible Category/SubType strings
// expected by the TCK error assertions and embeds the first underlying
// ScopeError so callers can recover the source position via [errors.As].
//
// SemanticError implements the error interface; its message is the message
// of the first wrapped ScopeError, prefixed with the Bolt category.
//
// Concurrency: SemanticError values are immutable after construction; safe
// for concurrent reads.
type SemanticError struct {
	// Category is the Bolt error category ("SyntaxError" or "TypeError").
	Category string
	// SubType is the Bolt error sub-type (e.g. "UndefinedVariable").
	SubType string
	// Errors holds every scope violation reported by [Analyse] in source
	// order. Always non-empty when SemanticError is non-nil.
	Errors []ScopeError
}

// Error implements the error interface. The format is:
//
//	"cypher: <Category>.<SubType>: <first underlying ScopeError message>"
func (e *SemanticError) Error() string {
	if len(e.Errors) == 0 {
		return fmt.Sprintf("cypher: %s.%s", e.Category, e.SubType)
	}
	return fmt.Sprintf("cypher: %s.%s: %s", e.Category, e.SubType, e.Errors[0].Error())
}

// Unwrap returns the first underlying [ScopeError] so [errors.As] can recover
// it. Only the first error is exposed because errors.Unwrap is single-valued;
// callers needing the full set should read [SemanticError.Errors] directly.
func (e *SemanticError) Unwrap() error {
	if len(e.Errors) == 0 {
		return nil
	}
	return &e.Errors[0]
}

// boltMapping pairs an ErrorKind with its TCK Category/SubType. The ordering
// in [kindMappings] defines mapping precedence when an analyser run produces
// more than one kind: the first matching entry wins.
type boltMapping struct {
	Kind     ErrorKind
	Category string
	SubType  string
}

// kindMappings is the canonical [ErrorKind] → Bolt mapping. Order matters:
// entries earlier in the slice win when multiple kinds appear in the same
// analyser output (see [MapToBolt]). The rationale for each row is:
//
//   - KindUndefinedVar and KindScopeLeak both surface as "variable not in
//     scope" to the user; the TCK consistently expects SyntaxError /
//     UndefinedVariable for them.
//   - KindRedeclaration is the analyser's signal that a name has been
//     introduced twice with conflicting roles (e.g. a node variable reused
//     as a relationship variable). The TCK label is VariableTypeConflict.
var kindMappings = []boltMapping{
	{Kind: KindUndefinedVar, Category: CategorySyntaxError, SubType: SubTypeUndefinedVariable},
	{Kind: KindScopeLeak, Category: CategorySyntaxError, SubType: SubTypeUndefinedVariable},
	{Kind: KindRedeclaration, Category: CategorySyntaxError, SubType: SubTypeVariableTypeConflict},
}

// MapToBolt converts a slice of [ScopeError]s into a single [*SemanticError]
// tagged with the Bolt category/sub-type the TCK expects. It returns nil
// when errs is empty.
//
// When the slice contains multiple kinds the precedence in [kindMappings]
// decides which (Category, SubType) pair labels the wrapper; the full error
// slice is preserved in [SemanticError.Errors] regardless of which mapping
// was chosen, so callers retain visibility into every violation.
//
// Unknown kinds fall back to ("SyntaxError", "SemanticError") so the engine
// never returns an unmapped sema failure.
func MapToBolt(errs []ScopeError) *SemanticError {
	if len(errs) == 0 {
		return nil
	}
	for _, m := range kindMappings {
		for _, e := range errs {
			if e.Kind == m.Kind {
				return &SemanticError{
					Category: m.Category,
					SubType:  m.SubType,
					Errors:   errs,
				}
			}
		}
	}
	// Unknown ErrorKind: fall back to a generic SyntaxError envelope so the
	// engine still produces a typed error instead of silently dropping the
	// analyser's report.
	return &SemanticError{
		Category: CategorySyntaxError,
		SubType:  "SemanticError",
		Errors:   errs,
	}
}
