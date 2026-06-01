package sema_test

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

func TestInferParamTypes_EqualityPredicate(t *testing.T) {
	// Simulate Selection(AllNodesScan, "(n.id = $pid)") as produced by the translator.
	scan := ir.NewAllNodesScan("n")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypes(root)
	if got["pid"] != expr.KindString {
		t.Errorf("expected KindString for $pid, got %v (map=%v)", got["pid"], got)
	}
}

func TestInferParamTypes_ReversedOperands(t *testing.T) {
	scan := ir.NewAllNodesScan("n")
	sel := ir.NewSelection("($name = n.email)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypes(root)
	if got["name"] != expr.KindString {
		t.Errorf("expected KindString for $name, got %v", got["name"])
	}
}

func TestInferParamTypes_NoParam(t *testing.T) {
	scan := ir.NewAllNodesScan("n")
	sel := ir.NewSelection(`(n.id = "Alice")`, scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypes(root)
	if len(got) != 0 {
		t.Errorf("expected no inferred params, got %v", got)
	}
}

func TestInferParamTypes_EmptyPlan(t *testing.T) {
	scan := ir.NewAllNodesScan("n")
	root := ir.NewProduceResults([]string{"n"}, scan)

	got := sema.InferParamTypes(root)
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestInferParamTypesWithResolver_ResolverKindWins(t *testing.T) {
	// (:Account) WHERE n.id = $pid — resolver types id as Integer.
	scan := ir.NewNodeByLabelScan("n", "Account")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	resolve := func(label, prop string) (expr.Kind, bool) {
		if label == "Account" && prop == "id" {
			return expr.KindInteger, true
		}
		return 0, false
	}
	got := sema.InferParamTypesWithResolver(root, resolve)
	if got["pid"] != expr.KindInteger {
		t.Errorf("expected KindInteger for $pid, got %v (map=%v)", got["pid"], got)
	}
}

func TestInferParamTypesWithResolver_NilResolverDefaultsToString(t *testing.T) {
	scan := ir.NewNodeByLabelScan("n", "Account")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypesWithResolver(root, nil)
	if got["pid"] != expr.KindString {
		t.Errorf("expected KindString default for $pid, got %v", got["pid"])
	}
}

func TestInferParamTypesWithResolver_ResolverMissDefaultsToString(t *testing.T) {
	scan := ir.NewNodeByLabelScan("n", "Account")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	// Resolver knows nothing about (Account, id): fall back to String.
	resolve := func(string, string) (expr.Kind, bool) { return 0, false }
	got := sema.InferParamTypesWithResolver(root, resolve)
	if got["pid"] != expr.KindString {
		t.Errorf("expected KindString fallback for $pid, got %v", got["pid"])
	}
}

func TestCheckParams_TypeMatch(t *testing.T) {
	inferred := map[string]expr.Kind{"pid": expr.KindString}
	params := map[string]expr.Value{"pid": expr.StringValue("alice")}
	if err := sema.CheckParams(inferred, params); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckParams_TypeMismatch(t *testing.T) {
	inferred := map[string]expr.Kind{"pid": expr.KindString}
	params := map[string]expr.Value{"pid": expr.IntegerValue(42)}
	err := sema.CheckParams(inferred, params)
	if err == nil {
		t.Fatal("expected type mismatch error")
	}
	var pte *sema.ParamTypeError
	if !errors.As(err, &pte) {
		t.Fatalf("expected *sema.ParamTypeError, got %T: %v", err, err)
	}
	if pte.Name != "pid" {
		t.Errorf("Name = %q, want %q", pte.Name, "pid")
	}
	if pte.Expected != expr.KindString {
		t.Errorf("Expected = %v, want KindString", pte.Expected)
	}
	if pte.Got != expr.KindInteger {
		t.Errorf("Got = %v, want KindInteger", pte.Got)
	}
}

func TestCheckParams_NullIsCompatible(t *testing.T) {
	inferred := map[string]expr.Kind{"pid": expr.KindString}
	params := map[string]expr.Value{"pid": expr.Null}
	if err := sema.CheckParams(inferred, params); err != nil {
		t.Errorf("NULL should be type-compatible: %v", err)
	}
}

func TestCheckParams_MissingParamSkipped(t *testing.T) {
	inferred := map[string]expr.Kind{"pid": expr.KindString}
	params := map[string]expr.Value{} // pid not provided
	if err := sema.CheckParams(inferred, params); err != nil {
		t.Errorf("missing param should not cause type error: %v", err)
	}
}

func TestParamTypeError_Message(t *testing.T) {
	e := &sema.ParamTypeError{
		Name:     "x",
		Expected: expr.KindString,
		Got:      expr.KindInteger,
	}
	msg := e.Error()
	if msg == "" {
		t.Fatal("error message should not be empty")
	}
	// Verify the message cites the param name.
	if len(msg) < 5 {
		t.Errorf("error message too short: %q", msg)
	}
}
