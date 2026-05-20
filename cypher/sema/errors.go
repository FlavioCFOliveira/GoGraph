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
