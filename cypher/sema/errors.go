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

	// KindInvalidArgumentType is reported when a literal expression of a
	// statically known non-boolean type is used as the operand of a logical
	// operator (AND / OR / XOR / NOT). Variables and other expressions whose
	// type is only known at runtime are not flagged.
	KindInvalidArgumentType ErrorKind = "INVALID_ARGUMENT_TYPE"

	// KindInvalidAggregation is reported when an aggregation function call
	// appears in an ORDER BY item but the surrounding projection does not
	// itself contain any aggregation. Aggregations only fold over groups
	// defined by the projection; standing alone in an ORDER BY is illegal.
	KindInvalidAggregation ErrorKind = "INVALID_AGGREGATION"

	// KindVariableAlreadyBound is reported when CREATE re-uses a previously
	// bound variable AND attempts to add new labels or properties to it.
	// openCypher 9 §3.5.1: a bound node may be referenced from CREATE (so
	// the pattern can describe edges around it) but cannot have its labels
	// or properties augmented.
	KindVariableAlreadyBound ErrorKind = "VARIABLE_ALREADY_BOUND"

	// KindColumnNameConflict is reported when a RETURN or WITH projection
	// declares two columns with the same output name (e.g.
	// `RETURN 1 AS a, 2 AS a`). openCypher 9 §3.3.3 rejects this at
	// compile time because the downstream consumer cannot disambiguate
	// the columns.
	KindColumnNameConflict ErrorKind = "COLUMN_NAME_CONFLICT"

	// KindUnknownFunction is reported when a function-call expression
	// names a function that is not registered in the engine's function
	// registry and is not a recognised aggregate (count, sum, avg, min,
	// max, collect, stdev, stdevp, percentileCont, percentileDisc).
	// openCypher 9 §6.1 requires compile-time rejection of unknown
	// function calls.
	KindUnknownFunction ErrorKind = "UNKNOWN_FUNCTION"

	// KindRelationshipUniqueness is reported when a relationship
	// variable is introduced more than once within the same path
	// pattern (e.g. `MATCH (a)-[r]->()-[r]->(a)`). openCypher 9
	// §3.3.1.2 forbids this: a single path pattern cannot bind two
	// distinct edges to the same relationship name.
	KindRelationshipUniqueness ErrorKind = "RELATIONSHIP_UNIQUENESS"
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

// invalidBooleanOperandError constructs a KindInvalidArgumentType ScopeError
// for a non-boolean literal used as the operand of a logical operator.
func invalidBooleanOperandError(op, gotKind string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindInvalidArgumentType,
		Pos:     pos,
		Message: fmt.Sprintf("operator %q expects Boolean operands, got %s literal", op, gotKind),
	}
}

// invalidAggregationError constructs a KindInvalidAggregation ScopeError
// for an aggregation used in ORDER BY without a matching projection-level
// aggregation.
func invalidAggregationError(pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindInvalidAggregation,
		Pos:     pos,
		Message: "aggregation in ORDER BY requires a matching aggregation in the projection",
	}
}

// variableAlreadyBoundError constructs a KindVariableAlreadyBound ScopeError
// for a CREATE node pattern that re-uses a bound variable AND attempts to
// declare new labels or properties on it.
func variableAlreadyBoundError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindVariableAlreadyBound,
		Pos:     pos,
		Message: fmt.Sprintf("variable %q is already bound; CREATE cannot augment its labels or properties", name),
	}
}

// columnNameConflictError constructs a KindColumnNameConflict ScopeError
// for a RETURN/WITH projection that declares duplicate output column names.
func columnNameConflictError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindColumnNameConflict,
		Pos:     pos,
		Message: fmt.Sprintf("duplicate column name %q in projection", name),
	}
}

// unknownFunctionError constructs a KindUnknownFunction ScopeError for a
// function-call expression whose name does not resolve to any registered
// scalar built-in or recognised aggregate.
func unknownFunctionError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindUnknownFunction,
		Pos:     pos,
		Message: fmt.Sprintf("unknown function %q", name),
	}
}

// relationshipUniquenessError constructs a KindRelationshipUniqueness
// ScopeError for a relationship variable introduced twice within the
// same path pattern.
func relationshipUniquenessError(name string, pos ast.Position) *ScopeError {
	return &ScopeError{
		Kind:    KindRelationshipUniqueness,
		Pos:     pos,
		Message: fmt.Sprintf("relationship variable %q cannot be used twice in the same path pattern", name),
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

	// SubTypeInvalidAggregation is the TCK sub-type for an aggregation
	// used in ORDER BY when the projection does not itself aggregate.
	SubTypeInvalidAggregation = "InvalidAggregation"

	// SubTypeVariableAlreadyBound is the TCK sub-type for CREATE on a
	// previously bound variable with new labels or properties.
	SubTypeVariableAlreadyBound = "VariableAlreadyBound"

	// SubTypeColumnNameConflict is the TCK sub-type for a RETURN/WITH
	// projection that declares duplicate output column names.
	SubTypeColumnNameConflict = "ColumnNameConflict"

	// SubTypeUnknownFunction is the canonical TCK sub-type for
	// references to functions the engine does not implement.
	SubTypeUnknownFunction = "UnknownFunction"

	// SubTypeRelationshipUniqueness is the canonical TCK sub-type for
	// a relationship variable introduced more than once in the same
	// path pattern.
	SubTypeRelationshipUniqueness = "RelationshipUniquenessViolation"
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
	{Kind: KindInvalidArgumentType, Category: CategorySyntaxError, SubType: SubTypeInvalidArgumentType},
	{Kind: KindInvalidAggregation, Category: CategorySyntaxError, SubType: SubTypeInvalidAggregation},
	{Kind: KindVariableAlreadyBound, Category: CategorySyntaxError, SubType: SubTypeVariableAlreadyBound},
	{Kind: KindColumnNameConflict, Category: CategorySyntaxError, SubType: SubTypeColumnNameConflict},
	{Kind: KindUnknownFunction, Category: CategorySyntaxError, SubType: SubTypeUnknownFunction},
	{Kind: KindRelationshipUniqueness, Category: CategorySyntaxError, SubType: SubTypeRelationshipUniqueness},
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
