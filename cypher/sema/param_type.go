package sema

import (
	"fmt"
	"strings"

	"gograph/cypher/expr"
	"gograph/cypher/ir"
)

// param_type.go — parameter type inference and runtime binding validation.
//
// InferParamTypes walks an IR plan tree and collects the expected expr.Kind for
// every named parameter that appears in an equality predicate of the form
//
//	n.prop = $name   or   $name = n.prop
//
// The inferred kind is KindString by default (most property lookups use string
// keys). Callers can use the returned map with CheckParams to validate that
// the params map supplied at Run time is type-compatible before query execution.

// ParamTypeError is returned by [CheckParams] when a parameter value's Kind
// does not match the expected Kind inferred from the query context.
type ParamTypeError struct {
	// Name is the Cypher parameter name (without the leading $).
	Name string
	// Expected is the Kind inferred from the expression context.
	Expected expr.Kind
	// Got is the Kind of the value provided by the caller.
	Got expr.Kind
}

func (e *ParamTypeError) Error() string {
	return fmt.Sprintf("cypher: parameter $%s: expected %s value, got %s",
		e.Name, e.Expected, e.Got)
}

// InferParamTypes walks plan looking for Selection nodes whose predicate is an
// equality comparison involving a parameter reference ($name) and a property
// access (n.prop). It returns a map from parameter name (without $) to the
// expected expr.Kind.
//
// When the same parameter appears in multiple incompatible contexts the first
// encountered wins. Parameters used in non-inferrable positions are omitted.
func InferParamTypes(plan ir.LogicalPlan) map[string]expr.Kind {
	result := make(map[string]expr.Kind)
	inferFromPlan(plan, result)
	return result
}

func inferFromPlan(plan ir.LogicalPlan, out map[string]expr.Kind) {
	if plan == nil {
		return
	}
	if sel, ok := plan.(*ir.Selection); ok {
		inferFromPredicate(sel.Predicate, out)
	}
	for _, child := range plan.Children() {
		inferFromPlan(child, out)
	}
}

// inferFromPredicate parses the opaque predicate string for patterns of the
// form "(n.prop = $name)" or "($name = n.prop)" and records
// name → KindString as the expected type.
//
// The string form produced by [ast.BinaryOp.String] wraps the expression in
// parentheses: "(left op right)". We strip outer parens and match the = case.
func inferFromPredicate(pred string, out map[string]expr.Kind) {
	pred = strings.TrimSpace(pred)
	// Strip wrapping parens produced by BinaryOp.String().
	if strings.HasPrefix(pred, "(") && strings.HasSuffix(pred, ")") {
		pred = pred[1 : len(pred)-1]
	}

	// Split on " = " to get left and right operands.
	idx := strings.Index(pred, " = ")
	if idx < 0 {
		return
	}
	left := strings.TrimSpace(pred[:idx])
	right := strings.TrimSpace(pred[idx+3:])

	// Pattern 1: n.prop = $name
	if strings.ContainsRune(left, '.') && strings.HasPrefix(right, "$") {
		name := right[1:]
		if _, seen := out[name]; !seen {
			out[name] = expr.KindString
		}
		return
	}
	// Pattern 2: $name = n.prop
	if strings.HasPrefix(left, "$") && strings.ContainsRune(right, '.') {
		name := left[1:]
		if _, seen := out[name]; !seen {
			out[name] = expr.KindString
		}
	}
}

// CheckParams validates that every parameter in inferred also appears in
// params with a compatible Kind. It returns a [*ParamTypeError] for the first
// mismatch found, or nil when all checked parameters are type-compatible.
//
// Parameters present in params but absent from inferred are silently accepted
// (they may be referenced in positions the pass does not yet analyse).
func CheckParams(inferred map[string]expr.Kind, params map[string]expr.Value) error {
	for name, wantKind := range inferred {
		v, ok := params[name]
		if !ok {
			// Missing parameter — the executor will resolve it to NULL; not a type error.
			continue
		}
		if expr.IsNull(v) {
			// NULL is compatible with any expected type per Cypher three-valued logic.
			continue
		}
		if v.Kind() != wantKind {
			return &ParamTypeError{Name: name, Expected: wantKind, Got: v.Kind()}
		}
	}
	return nil
}
