package sema

import (
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// param_type.go — parameter type inference and runtime binding validation.
//
// InferParamTypes walks an IR plan tree and collects the expected expr.Kind for
// every named parameter that appears in an equality predicate of the form
//
//	n.prop = $name   or   $name = n.prop
//
// The query text alone does not reveal the type of n.prop: it depends on the
// data stored under that property. The only authoritative, declared type signal
// the engine has is an index on (label, prop) — an int64 hash index proves the
// property is Integer, a string hash index proves String, and so on. Callers
// supply a PropTypeResolver that maps (label, prop) to the indexed key kind;
// only when the resolver returns ok=true is the parameter recorded. Parameters
// whose type cannot be determined are omitted from the map and thus accepted
// unconditionally by CheckParams.
//
// Callers use the returned map with CheckParams to validate that the params map
// supplied at Run time is type-compatible before query execution.

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

// Error implements the error interface.
func (e *ParamTypeError) Error() string {
	return fmt.Sprintf("cypher: parameter $%s: expected %s value, got %s",
		e.Name, e.Expected, e.Got)
}

// PropTypeResolver returns the declared expr.Kind of a (nodeLabel, property)
// pair when an authoritative type signal exists for it — in practice, an index
// whose key type is known. label is empty when the property is read from an
// unlabelled scan. ok is false when no type is known, in which case the caller
// keeps its conservative default.
//
// A resolver must be a pure read-only lookup; InferParamTypesWithResolver may
// call it once per inferrable predicate.
type PropTypeResolver func(label, property string) (kind expr.Kind, ok bool)

// InferParamTypes walks plan looking for Selection nodes whose predicate is an
// equality comparison involving a parameter reference ($name) and a property
// access (n.prop). It returns a map from parameter name (without $) to the
// expected expr.Kind. Parameters whose type cannot be determined are omitted.
//
// It is equivalent to InferParamTypesWithResolver(plan, nil) and is retained
// for callers that have no index information to offer.
func InferParamTypes(plan ir.LogicalPlan) map[string]expr.Kind {
	return InferParamTypesWithResolver(plan, nil)
}

// InferParamTypesWithResolver behaves like InferParamTypes but consults resolve
// to determine the expected kind of a parameter compared against a property.
// A parameter is recorded only when resolve returns ok=true; when resolve is
// nil or returns false the parameter is omitted (type unknown, accepted freely).
//
// When the same parameter appears in multiple inferrable contexts the first
// recorded kind wins. Parameters used in non-inferrable positions are omitted.
func InferParamTypesWithResolver(plan ir.LogicalPlan, resolve PropTypeResolver) map[string]expr.Kind {
	result := make(map[string]expr.Kind)
	inferFromPlan(plan, resolve, result)
	return result
}

func inferFromPlan(plan ir.LogicalPlan, resolve PropTypeResolver, out map[string]expr.Kind) {
	if plan == nil {
		return
	}
	if sel, ok := plan.(*ir.Selection); ok {
		inferFromPredicate(sel.Predicate, scanLeafLabel(sel.Child), resolve, out)
	}
	for _, child := range plan.Children() {
		inferFromPlan(child, resolve, out)
	}
}

// scanLeafLabel returns the node label of a bare scan leaf directly beneath a
// Selection, or "" for an unlabelled AllNodesScan or any non-scan child. The
// label lets the resolver disambiguate which index backs the property.
func scanLeafLabel(child ir.LogicalPlan) string {
	switch c := child.(type) {
	case *ir.NodeByLabelScan:
		return c.Label
	default:
		return ""
	}
}

// inferFromPredicate parses the opaque predicate string for patterns of the
// form "(n.prop = $name)" or "($name = n.prop)" and records the expected kind
// for name. The kind is taken from resolve(label, prop) when known, else
// KindString.
//
// The string form produced by [ast.BinaryOp.String] wraps the expression in
// parentheses: "(left op right)". We strip outer parens and match the = case.
func inferFromPredicate(pred, label string, resolve PropTypeResolver, out map[string]expr.Kind) {
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
	if prop, isProp := propKeyOf(left); isProp && strings.HasPrefix(right, "$") {
		recordParam(out, right[1:], label, prop, resolve)
		return
	}
	// Pattern 2: $name = n.prop
	if prop, isProp := propKeyOf(right); isProp && strings.HasPrefix(left, "$") {
		recordParam(out, left[1:], label, prop, resolve)
	}
}

// propKeyOf returns the property key of a "var.key" operand and true, or
// ("", false) when operand is not a property access. Only the final dotted
// segment is treated as the key, mirroring [ast.Property.String].
func propKeyOf(operand string) (key string, ok bool) {
	i := strings.LastIndexByte(operand, '.')
	if i < 0 || i == len(operand)-1 {
		return "", false
	}
	return operand[i+1:], true
}

// recordParam records name → kind only when resolve returns an authoritative
// kind for (label, prop). When resolve is nil or returns ok=false the parameter
// is left out of out so CheckParams accepts it unconditionally. The first kind
// recorded for a given name wins.
func recordParam(out map[string]expr.Kind, name, label, prop string, resolve PropTypeResolver) {
	if _, seen := out[name]; seen {
		return
	}
	if resolve == nil {
		return
	}
	k, ok := resolve(label, prop)
	if !ok {
		return
	}
	out[name] = k
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
