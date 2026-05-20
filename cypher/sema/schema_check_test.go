package sema_test

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/sema"
	"gograph/graph/lpg"
	"gograph/graph/lpg/schema"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// makeSchema builds a schema with "age" → PropInt64 and "name" → PropString.
func makeSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s := schema.New(nil, nil)
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty age: %v", err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		t.Fatalf("RegisterProperty name: %v", err)
	}
	return s
}

// propAccess builds n.key.
func propAccess(varName, key string) *ast.Property {
	return &ast.Property{
		Receiver: &ast.Variable{Name: varName, Pos: pos(1, 1)},
		Key:      key,
	}
}

// compareExpr builds a BinaryOp with operator "=".
func compareExpr(left, right ast.Expression) *ast.BinaryOp {
	return &ast.BinaryOp{
		Left:     left,
		Operator: "=",
		Right:    right,
		Pos:      pos(1, 10),
	}
}

// whereQuery builds a SingleQuery: MATCH (n) WHERE pred RETURN n.
func whereQuery(pred ast.Expression) *ast.SingleQuery {
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 7)},
					},
				}}},
				Where: &ast.Where{Predicate: pred},
			},
		},
		Return: returnVar("n"),
	}
}

// assertSchemaErrors checks that CheckSchema returns exactly wantCount errors.
func assertSchemaErrors(t *testing.T, q ast.Query, sch *schema.Schema, wantCount int) []sema.SchemaError {
	t.Helper()
	errs := sema.CheckSchema(q, sch)
	if len(errs) != wantCount {
		t.Fatalf("CheckSchema: want %d error(s), got %d: %v", wantCount, len(errs), errs)
	}
	return errs
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Nil schema → no-op
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_NilSchema_NoOp(t *testing.T) {
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.StringLiteral{Value: "hello"},
	))
	assertSchemaErrors(t, q, nil, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Mismatches — error expected
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_IntProp_StringLiteral_Error(t *testing.T) {
	// n.age = "hello"  — age is PropInt64
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.StringLiteral{Value: "hello"},
	))
	errs := assertSchemaErrors(t, q, makeSchema(t), 1)
	if errs[0].PropertyName != "age" {
		t.Errorf("PropertyName = %q, want %q", errs[0].PropertyName, "age")
	}
	if errs[0].DeclaredKind != lpg.PropInt64 {
		t.Errorf("DeclaredKind = %d, want PropInt64", errs[0].DeclaredKind)
	}
	if errs[0].UsedAs != sema.TypeString {
		t.Errorf("UsedAs = %v, want TypeString", errs[0].UsedAs)
	}
	if errs[0].Hint == "" {
		t.Error("Hint must be non-empty")
	}
}

func TestCheckSchema_StringProp_IntLiteral_Error(t *testing.T) {
	// n.name = 42  — name is PropString
	q := whereQuery(compareExpr(
		propAccess("n", "name"),
		&ast.IntLiteral{Value: 42},
	))
	errs := assertSchemaErrors(t, q, makeSchema(t), 1)
	if errs[0].PropertyName != "name" {
		t.Errorf("PropertyName = %q, want %q", errs[0].PropertyName, "name")
	}
	if errs[0].DeclaredKind != lpg.PropString {
		t.Errorf("DeclaredKind = %d, want PropString", errs[0].DeclaredKind)
	}
	if errs[0].UsedAs != sema.TypeInteger {
		t.Errorf("UsedAs = %v, want TypeInteger", errs[0].UsedAs)
	}
}

func TestCheckSchema_IntProp_FloatLiteral_Error(t *testing.T) {
	// n.age = 3.14  — age is PropInt64; Float does not widen to Int
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.FloatLiteral{Value: 3.14},
	))
	assertSchemaErrors(t, q, makeSchema(t), 1)
}

func TestCheckSchema_StringProp_BoolLiteral_Error(t *testing.T) {
	// n.name = true  — name is PropString
	q := whereQuery(compareExpr(
		propAccess("n", "name"),
		&ast.BoolLiteral{Value: true},
	))
	assertSchemaErrors(t, q, makeSchema(t), 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Correct type matches — no error
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_IntProp_IntLiteral_OK(t *testing.T) {
	// n.age = 25  — age is PropInt64
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.IntLiteral{Value: 25},
	))
	assertSchemaErrors(t, q, makeSchema(t), 0)
}

func TestCheckSchema_StringProp_StringLiteral_OK(t *testing.T) {
	// n.name = "Alice"  — name is PropString
	q := whereQuery(compareExpr(
		propAccess("n", "name"),
		&ast.StringLiteral{Value: "Alice"},
	))
	assertSchemaErrors(t, q, makeSchema(t), 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Property not in schema → no error (partial schema / warning-only)
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_UnknownProperty_NoError(t *testing.T) {
	// n.score = "foo"  — score is not registered
	q := whereQuery(compareExpr(
		propAccess("n", "score"),
		&ast.StringLiteral{Value: "foo"},
	))
	assertSchemaErrors(t, q, makeSchema(t), 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Empty schema → always no error
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_EmptySchema_NoError(t *testing.T) {
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.StringLiteral{Value: "wrong"},
	))
	assertSchemaErrors(t, q, schema.New(nil, nil), 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Literal on left side, property on right (commutative)
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_LiteralLeft_PropRight_Mismatch(t *testing.T) {
	// "hello" = n.age  — same mismatch, sides flipped
	q := whereQuery(&ast.BinaryOp{
		Left:     &ast.StringLiteral{Value: "hello"},
		Operator: "=",
		Right:    propAccess("n", "age"),
		Pos:      pos(1, 10),
	})
	assertSchemaErrors(t, q, makeSchema(t), 1)
}

func TestCheckSchema_LiteralLeft_PropRight_OK(t *testing.T) {
	// 42 = n.age  — correct type, sides flipped
	q := whereQuery(&ast.BinaryOp{
		Left:     &ast.IntLiteral{Value: 42},
		Operator: "=",
		Right:    propAccess("n", "age"),
		Pos:      pos(1, 10),
	})
	assertSchemaErrors(t, q, makeSchema(t), 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Null literal is always compatible (three-valued logic)
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_NullLiteral_NoError(t *testing.T) {
	// n.age = null  — null is always compatible
	q := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.NullLiteral{},
	))
	assertSchemaErrors(t, q, makeSchema(t), 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. MultiQuery (UNION) — both branches are checked
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_MultiQuery_ErrorInSecondBranch(t *testing.T) {
	branch1 := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.IntLiteral{Value: 1},
	))
	branch2 := whereQuery(compareExpr(
		propAccess("n", "age"),
		&ast.StringLiteral{Value: "bad"},
	))
	q := &ast.MultiQuery{Parts: []*ast.SingleQuery{branch1, branch2}}
	assertSchemaErrors(t, q, makeSchema(t), 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. SchemaError implements error interface and has useful message
// ─────────────────────────────────────────────────────────────────────────────

func TestSchemaError_ErrorString(t *testing.T) {
	errs := sema.CheckSchema(
		whereQuery(compareExpr(
			propAccess("n", "age"),
			&ast.StringLiteral{Value: "oops"},
		)),
		makeSchema(t),
	)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %d", len(errs))
	}
	msg := errs[0].Error()
	if msg == "" {
		t.Fatal("SchemaError.Error() must return non-empty string")
	}
	for _, want := range []string{"age", "Integer", "String"} {
		found := false
		for i := 0; i+len(want) <= len(msg); i++ {
			if msg[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SchemaError.Error() = %q; missing %q", msg, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. Operator sensitivity — non-comparison operators are skipped
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckSchema_NonComparisonOperator_NoError(t *testing.T) {
	// n.age + "hello"  — "+" is arithmetic, not a comparison; not a schema error
	q := whereQuery(&ast.BinaryOp{
		Left:     propAccess("n", "age"),
		Operator: "+",
		Right:    &ast.StringLiteral{Value: "hello"},
		Pos:      pos(1, 10),
	})
	assertSchemaErrors(t, q, makeSchema(t), 0)
}
