package sema_test

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

func TestInferParamTypes_EqualityPredicate(t *testing.T) {
	// No resolver: type unknown, parameter must be omitted.
	scan := ir.NewAllNodesScan("n")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypes(root)
	if _, present := got["pid"]; present {
		t.Errorf("expected $pid omitted when type unknown, got map=%v", got)
	}
}

func TestInferParamTypes_ReversedOperands(t *testing.T) {
	// No resolver: type unknown, parameter must be omitted.
	scan := ir.NewAllNodesScan("n")
	sel := ir.NewSelection("($name = n.email)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypes(root)
	if _, present := got["name"]; present {
		t.Errorf("expected $name omitted when type unknown, got map=%v", got)
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

func TestInferParamTypesWithResolver_NilResolverOmitsParam(t *testing.T) {
	// Nil resolver: type unknown, parameter must be omitted (not defaulted to String).
	scan := ir.NewNodeByLabelScan("n", "Account")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	got := sema.InferParamTypesWithResolver(root, nil)
	if _, present := got["pid"]; present {
		t.Errorf("expected $pid omitted for nil resolver, got map=%v", got)
	}
}

func TestInferParamTypesWithResolver_ResolverMissOmitsParam(t *testing.T) {
	// Resolver returns ok=false: type unknown, parameter must be omitted.
	scan := ir.NewNodeByLabelScan("n", "Account")
	sel := ir.NewSelection("(n.id = $pid)", scan)
	root := ir.NewProduceResults([]string{"n"}, sel)

	resolve := func(string, string) (expr.Kind, bool) { return 0, false }
	got := sema.InferParamTypesWithResolver(root, resolve)
	if _, present := got["pid"]; present {
		t.Errorf("expected $pid omitted when resolver returns ok=false, got map=%v", got)
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

// TestCheckParams_NonStringParams is the gate test for the fix that omits
// parameters of unknown type rather than defaulting them to KindString.
// Before the fix, Integer/Float/Bool params on unindexed properties would be
// rejected because recordParam defaulted to KindString.
func TestCheckParams_NonStringParams(t *testing.T) {
	// resolveString returns KindString only for (Person, name); all others unknown.
	resolveString := func(label, prop string) (expr.Kind, bool) {
		if label == "Person" && prop == "name" {
			return expr.KindString, true
		}
		return 0, false
	}

	buildPlan := func(pred, label string) ir.LogicalPlan {
		var child ir.LogicalPlan
		if label != "" {
			child = ir.NewNodeByLabelScan("n", label)
		} else {
			child = ir.NewAllNodesScan("n")
		}
		sel := ir.NewSelection(pred, child)
		return ir.NewProduceResults([]string{"n"}, sel)
	}

	tests := []struct {
		name      string
		pred      string
		label     string
		param     string
		value     expr.Value
		wantErr   bool
		wantInMap bool // whether the param should appear in the inferred map at all
	}{
		// Unindexed properties: type unknown → param omitted → accepted freely.
		{
			name:      "integer param on unindexed prop accepted",
			pred:      "(n.age = $v)",
			label:     "Person",
			param:     "v",
			value:     expr.IntegerValue(30),
			wantErr:   false,
			wantInMap: false,
		},
		{
			name:      "float param on unindexed prop accepted",
			pred:      "(n.score = $v)",
			label:     "Person",
			param:     "v",
			value:     expr.FloatValue(3.14),
			wantErr:   false,
			wantInMap: false,
		},
		{
			name:      "bool param on unindexed prop accepted",
			pred:      "(n.active = $v)",
			label:     "Person",
			param:     "v",
			value:     expr.BoolValue(true),
			wantErr:   false,
			wantInMap: false,
		},
		// No label → resolver cannot identify index → param omitted → accepted.
		{
			name:      "integer param on label-less scan accepted",
			pred:      "(n.age = $v)",
			label:     "",
			param:     "v",
			value:     expr.IntegerValue(99),
			wantErr:   false,
			wantInMap: false,
		},
		// Indexed String property: resolver returns KindString → Integer param rejected.
		{
			name:      "integer param on indexed string prop rejected",
			pred:      "(n.name = $v)",
			label:     "Person",
			param:     "v",
			value:     expr.IntegerValue(42),
			wantErr:   true,
			wantInMap: true,
		},
		// Indexed String property: matching String param accepted.
		{
			name:      "string param on indexed string prop accepted",
			pred:      "(n.name = $v)",
			label:     "Person",
			param:     "v",
			value:     expr.StringValue("Alice"),
			wantErr:   false,
			wantInMap: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildPlan(tc.pred, tc.label)
			inferred := sema.InferParamTypesWithResolver(plan, resolveString)

			_, inMap := inferred[tc.param]
			if inMap != tc.wantInMap {
				t.Errorf("param %q in inferred map = %v, want %v (map=%v)", tc.param, inMap, tc.wantInMap, inferred)
			}

			params := map[string]expr.Value{tc.param: tc.value}
			err := sema.CheckParams(inferred, params)
			if tc.wantErr && err == nil {
				t.Errorf("expected type error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
