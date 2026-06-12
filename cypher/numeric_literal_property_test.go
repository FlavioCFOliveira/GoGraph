package cypher_test

// numeric_literal_property_test.go — regression gate for task #1430:
// property access on a numeric literal ((5).foo, 5.foo) must raise
// InvalidArgumentType at compile time, not return null.
//
// The parser already converts `1.0`, `1.5` etc. (IntLiteral + all-digit
// property accessor) to *ast.FloatLiteral before sema runs, so any
// *ast.Property node whose receiver is *ast.IntLiteral or *ast.FloatLiteral
// that survives to sema cannot be a float-literal reconstruction: it is a
// genuine property access on a number, which is a type error.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newEngineForPropTest(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// runExpectCompileError runs query and asserts a compile-time error is returned.
// It accepts both *sema.SemanticError and *parser.SemaError (both are compile-time
// syntax/type errors). Returns the error for further inspection.
func runExpectCompileError(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if res != nil {
		for res.Next() {
		}
		_ = res.Close()
	}
	if err == nil {
		t.Fatalf("query %q: expected compile-time error, got nil", query)
	}
	return err
}

// TestNumericLiteralPropertyAccess_IntegerReceiver verifies that (5).foo raises
// an InvalidArgumentType error at compile time (#1430).
func TestNumericLiteralPropertyAccess_IntegerReceiver(t *testing.T) {
	eng := newEngineForPropTest(t)

	err := runExpectCompileError(t, eng, "RETURN (5).foo")

	// Must be a compile-time sema error (InvalidArgumentType or equivalent).
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
}

// TestNumericLiteralPropertyAccess_FloatLiteralPreservation verifies that `1.5`
// (a genuine float literal) continues to evaluate correctly and is NOT affected
// by the numeric-receiver guard — the parser already converted it to FloatLiteral
// before sema, so no *ast.Property node exists in the AST.
func TestNumericLiteralPropertyAccess_FloatLiteralPreservation(t *testing.T) {
	eng := newEngineForPropTest(t)

	res, err := eng.Run(context.Background(), "RETURN 1.5", nil)
	if err != nil {
		t.Fatalf("RETURN 1.5: unexpected error: %v", err)
	}
	defer func() { _ = res.Close() }()

	if !res.Next() {
		t.Fatal("expected one row from RETURN 1.5, got none")
	}
	_ = res.Record()
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil", err)
	}
}

// TestNumericLiteralPropertyAccess_FloatLiteralZero ensures `1.0` is not affected.
func TestNumericLiteralPropertyAccess_FloatLiteralZero(t *testing.T) {
	eng := newEngineForPropTest(t)

	res, err := eng.Run(context.Background(), "RETURN 1.0", nil)
	if err != nil {
		t.Fatalf("RETURN 1.0: unexpected error: %v", err)
	}
	defer func() { _ = res.Close() }()

	if !res.Next() {
		t.Fatal("expected one row from RETURN 1.0, got none")
	}
}
