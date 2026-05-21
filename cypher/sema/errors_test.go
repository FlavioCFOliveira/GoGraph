package sema_test

import (
	"errors"
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/sema"
)

// TestMapToBolt_Nil verifies that an empty error slice maps to nil.
func TestMapToBolt_Nil(t *testing.T) {
	if got := sema.MapToBolt(nil); got != nil {
		t.Fatalf("MapToBolt(nil): expected nil, got %v", got)
	}
	if got := sema.MapToBolt([]sema.ScopeError{}); got != nil {
		t.Fatalf("MapToBolt(empty): expected nil, got %v", got)
	}
}

// TestMapToBolt_UndefinedVar verifies the KindUndefinedVar → SyntaxError
// /UndefinedVariable mapping and that the wrapped slice is preserved.
func TestMapToBolt_UndefinedVar(t *testing.T) {
	errs := []sema.ScopeError{
		{Kind: sema.KindUndefinedVar, Pos: ast.Position{Line: 1, Column: 8}, Message: `undefined variable "x"`},
	}
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("MapToBolt: expected non-nil SemanticError")
	}
	if se.Category != sema.CategorySyntaxError {
		t.Errorf("Category: got %q, want %q", se.Category, sema.CategorySyntaxError)
	}
	if se.SubType != sema.SubTypeUndefinedVariable {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeUndefinedVariable)
	}
	if len(se.Errors) != 1 {
		t.Fatalf("Errors length: got %d, want 1", len(se.Errors))
	}
	if se.Errors[0].Kind != sema.KindUndefinedVar {
		t.Errorf("wrapped kind: got %q, want %q", se.Errors[0].Kind, sema.KindUndefinedVar)
	}
}

// TestMapToBolt_Redeclaration verifies the KindRedeclaration →
// SyntaxError/VariableTypeConflict mapping.
func TestMapToBolt_Redeclaration(t *testing.T) {
	errs := []sema.ScopeError{
		{Kind: sema.KindRedeclaration, Pos: ast.Position{Line: 2, Column: 5}, Message: `variable "r" already declared`},
	}
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("MapToBolt: expected non-nil SemanticError")
	}
	if se.Category != sema.CategorySyntaxError {
		t.Errorf("Category: got %q, want %q", se.Category, sema.CategorySyntaxError)
	}
	if se.SubType != sema.SubTypeVariableTypeConflict {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeVariableTypeConflict)
	}
}

// TestMapToBolt_ScopeLeak verifies KindScopeLeak maps to UndefinedVariable
// (the user-facing symptom is identical).
func TestMapToBolt_ScopeLeak(t *testing.T) {
	errs := []sema.ScopeError{
		{Kind: sema.KindScopeLeak, Pos: ast.Position{Line: 3, Column: 1}, Message: `variable "x" not visible`},
	}
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("MapToBolt: expected non-nil SemanticError")
	}
	if se.SubType != sema.SubTypeUndefinedVariable {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeUndefinedVariable)
	}
}

// TestMapToBolt_Precedence verifies that when both UndefinedVar and
// Redeclaration are present the UndefinedVar mapping wins per
// kindMappings ordering (UndefinedVar is listed first).
func TestMapToBolt_Precedence(t *testing.T) {
	errs := []sema.ScopeError{
		{Kind: sema.KindRedeclaration, Pos: ast.Position{Line: 1, Column: 1}, Message: "redecl"},
		{Kind: sema.KindUndefinedVar, Pos: ast.Position{Line: 2, Column: 1}, Message: "undef"},
	}
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("MapToBolt: expected non-nil SemanticError")
	}
	if se.SubType != sema.SubTypeUndefinedVariable {
		t.Errorf("SubType under precedence: got %q, want %q (UndefinedVar wins)",
			se.SubType, sema.SubTypeUndefinedVariable)
	}
	// Both ScopeErrors must be preserved.
	if len(se.Errors) != 2 {
		t.Errorf("preserved errors: got %d, want 2", len(se.Errors))
	}
}

// TestMapToBolt_UnknownKindFallback verifies that an unknown ErrorKind
// falls back to a generic SyntaxError/SemanticError envelope rather than
// returning nil (which would silently drop the analyser's report).
func TestMapToBolt_UnknownKindFallback(t *testing.T) {
	errs := []sema.ScopeError{
		{Kind: sema.ErrorKind("UNKNOWN_FAKE"), Pos: ast.Position{}, Message: "?"},
	}
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("MapToBolt(unknown): expected non-nil fallback envelope")
	}
	if se.Category != sema.CategorySyntaxError {
		t.Errorf("fallback Category: got %q, want %q", se.Category, sema.CategorySyntaxError)
	}
	if se.SubType != "SemanticError" {
		t.Errorf("fallback SubType: got %q, want %q", se.SubType, "SemanticError")
	}
}

// TestSemanticError_Error verifies the Error() formatting.
func TestSemanticError_Error(t *testing.T) {
	se := &sema.SemanticError{
		Category: sema.CategorySyntaxError,
		SubType:  sema.SubTypeUndefinedVariable,
		Errors: []sema.ScopeError{
			{Kind: sema.KindUndefinedVar, Pos: ast.Position{Line: 1, Column: 8}, Message: `undefined variable "x"`},
		},
	}
	got := se.Error()
	const wantPrefix = "cypher: SyntaxError.UndefinedVariable: "
	if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("Error() = %q; want prefix %q", got, wantPrefix)
	}
}

// TestSemanticError_Unwrap verifies the first ScopeError is recoverable
// through errors.As.
func TestSemanticError_Unwrap(t *testing.T) {
	se := &sema.SemanticError{
		Category: sema.CategorySyntaxError,
		SubType:  sema.SubTypeUndefinedVariable,
		Errors: []sema.ScopeError{
			{Kind: sema.KindUndefinedVar, Pos: ast.Position{Line: 1, Column: 8}, Message: "u"},
		},
	}
	var inner *sema.ScopeError
	if !errors.As(se, &inner) {
		t.Fatal("errors.As(*SemanticError, **ScopeError): expected match")
	}
	if inner.Kind != sema.KindUndefinedVar {
		t.Errorf("recovered kind: got %q, want %q", inner.Kind, sema.KindUndefinedVar)
	}
}

// TestSemanticError_UnwrapEmpty verifies Unwrap returns nil for an
// empty error slice (the constructor never produces this, but the method
// must be defensive).
func TestSemanticError_UnwrapEmpty(t *testing.T) {
	se := &sema.SemanticError{Category: sema.CategorySyntaxError, SubType: "x"}
	if got := se.Unwrap(); got != nil {
		t.Errorf("Unwrap() with no Errors: got %v, want nil", got)
	}
	// Error() should still produce a meaningful prefix.
	if got := se.Error(); got == "" {
		t.Error("Error() with no Errors: must not be empty")
	}
}
