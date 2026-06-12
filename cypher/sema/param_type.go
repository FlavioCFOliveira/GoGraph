package sema

import (
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
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

// ErrParamMissing is returned by [CheckParamPresence] when a parameter
// referenced in the query has no corresponding entry in the params map supplied
// at execution time. The name field carries the parameter name without the
// leading '$'.
type ErrParamMissing struct {
	// Name is the Cypher parameter name (without the leading $).
	Name string
}

// Error implements the error interface.
func (e *ErrParamMissing) Error() string {
	return "cypher: ParameterMissing: MissingParameter: expected parameter $" + e.Name
}

// CollectParamNames walks a parsed query AST and returns the deduplicated,
// sorted set of every parameter name referenced in it (the bare name, without
// the leading '$'). The returned slice is always non-nil; an empty slice means
// no parameters are referenced.
//
// This is used by the engine to validate that every parameter the query
// references is present in the caller-supplied params map before execution
// begins, so callers receive a typed [*ErrParamMissing] instead of silently
// treating the missing parameter as NULL.
func CollectParamNames(q ast.Query) []string {
	seen := make(map[string]struct{})
	collectParamNamesQuery(q, seen)
	if len(seen) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	// Deterministic order: sort so the first missing parameter reported is
	// predictable and tests can rely on it.
	sortStrings(names)
	return names
}

// CheckParamPresence validates that every parameter name in refs is present in
// params. It returns a [*ErrParamMissing] for the first missing name found, or
// nil when all are present. refs is obtained from [CollectParamNames].
func CheckParamPresence(refs []string, params map[string]expr.Value) error {
	for _, name := range refs {
		if _, ok := params[name]; !ok {
			return &ErrParamMissing{Name: name}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AST walker — internal helpers
// ─────────────────────────────────────────────────────────────────────────────

func collectParamNamesQuery(q ast.Query, seen map[string]struct{}) {
	switch v := q.(type) {
	case *ast.SingleQuery:
		collectParamNamesSingle(v, seen)
	case *ast.MultiQuery:
		for _, part := range v.Parts {
			collectParamNamesSingle(part, seen)
		}
	}
}

func collectParamNamesSingle(q *ast.SingleQuery, seen map[string]struct{}) {
	if q == nil {
		return
	}
	for _, c := range q.ReadingClauses {
		collectParamNamesReadingClause(c, seen)
	}
	for _, c := range q.UpdatingClauses {
		collectParamNamesUpdatingClause(c, seen)
	}
	if q.Return != nil {
		collectParamNamesProjection(q.Return.Projection, seen)
	}
}

func collectParamNamesReadingClause(c ast.ReadingClause, seen map[string]struct{}) {
	switch v := c.(type) {
	case *ast.Match:
		collectParamNamesPattern(v.Pattern, seen)
		if v.Where != nil {
			collectParamNamesExpr(v.Where.Predicate, seen)
		}
	case *ast.OptionalMatch:
		collectParamNamesPattern(v.Pattern, seen)
		if v.Where != nil {
			collectParamNamesExpr(v.Where.Predicate, seen)
		}
	case *ast.Unwind:
		collectParamNamesExpr(v.Expr, seen)
	case *ast.Call:
		for _, a := range v.Args {
			collectParamNamesExpr(a, seen)
		}
	case *ast.With:
		collectParamNamesProjection(v.Projection, seen)
		if v.Where != nil {
			collectParamNamesExpr(v.Where.Predicate, seen)
		}
	}
}

func collectParamNamesUpdatingClause(c ast.UpdatingClause, seen map[string]struct{}) {
	switch v := c.(type) {
	case *ast.Create:
		collectParamNamesPattern(v.Pattern, seen)
	case *ast.Merge:
		collectParamNamesPathPattern(v.Pattern, seen)
		for _, item := range v.OnMatch {
			collectParamNamesSetItem(item, seen)
		}
		for _, item := range v.OnCreate {
			collectParamNamesSetItem(item, seen)
		}
	case *ast.Set:
		for _, item := range v.Items {
			collectParamNamesSetItem(item, seen)
		}
	case *ast.Delete:
		for _, e := range v.Expressions {
			collectParamNamesExpr(e, seen)
		}
	case *ast.DetachDelete:
		for _, e := range v.Expressions {
			collectParamNamesExpr(e, seen)
		}
	case *ast.Remove:
		for _, item := range v.Items {
			// RemoveItem.Target is the property expression or variable.
			collectParamNamesExpr(item.Target, seen)
		}
	case *ast.Call:
		for _, a := range v.Args {
			collectParamNamesExpr(a, seen)
		}
	}
}

func collectParamNamesProjection(p *ast.Projection, seen map[string]struct{}) {
	if p == nil {
		return
	}
	for _, item := range p.Items {
		collectParamNamesExpr(item.Expr, seen)
	}
	for _, o := range p.OrderBy {
		collectParamNamesExpr(o.Expr, seen)
	}
	collectParamNamesExpr(p.Skip, seen)
	collectParamNamesExpr(p.Limit, seen)
}

func collectParamNamesPattern(pat *ast.Pattern, seen map[string]struct{}) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		collectParamNamesPathPattern(pp, seen)
	}
}

func collectParamNamesPathPattern(pp *ast.PathPattern, seen map[string]struct{}) {
	if pp == nil {
		return
	}
	el := pp.Head
	for el != nil {
		if el.Node != nil && el.Node.Properties != nil {
			collectParamNamesExpr(el.Node.Properties, seen)
		}
		if el.Relationship != nil && el.Relationship.Properties != nil {
			collectParamNamesExpr(el.Relationship.Properties, seen)
		}
		el = el.Next
	}
}

func collectParamNamesSetItem(item *ast.SetItem, seen map[string]struct{}) {
	if item == nil {
		return
	}
	if item.Value != nil {
		collectParamNamesExpr(item.Value, seen)
	}
}

// collectParamNamesExpr recursively collects *ast.Parameter names from e.
func collectParamNamesExpr(e ast.Expression, seen map[string]struct{}) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.Parameter:
		seen[v.Name] = struct{}{}

	case *ast.BinaryOp:
		collectParamNamesExpr(v.Left, seen)
		collectParamNamesExpr(v.Right, seen)

	case *ast.UnaryOp:
		collectParamNamesExpr(v.Operand, seen)

	case *ast.Property:
		collectParamNamesExpr(v.Receiver, seen)

	case *ast.FunctionInvocation:
		for _, a := range v.Args {
			collectParamNamesExpr(a, seen)
		}

	case *ast.ListLiteral:
		for _, el := range v.Elements {
			collectParamNamesExpr(el, seen)
		}

	case *ast.MapLiteral:
		for _, val := range v.Values {
			collectParamNamesExpr(val, seen)
		}

	case *ast.MapProjection:
		collectParamNamesExpr(v.Subject, seen)
		for _, item := range v.Items {
			if item.Value != nil {
				collectParamNamesExpr(item.Value, seen)
			}
		}

	case *ast.CaseExpression:
		collectParamNamesExpr(v.Subject, seen)
		for _, alt := range v.Alternatives {
			collectParamNamesExpr(alt.Condition, seen)
			collectParamNamesExpr(alt.Consequent, seen)
		}
		collectParamNamesExpr(v.ElseExpr, seen)

	case *ast.ListComprehension:
		collectParamNamesExpr(v.Source, seen)
		collectParamNamesExpr(v.Predicate, seen)
		collectParamNamesExpr(v.Projection, seen)

	case *ast.PatternComprehension:
		collectParamNamesPathPattern(v.Pattern, seen)
		collectParamNamesExpr(v.Predicate, seen)
		collectParamNamesExpr(v.Projection, seen)

	case *ast.SubscriptExpr:
		collectParamNamesExpr(v.Expr, seen)
		collectParamNamesExpr(v.Index, seen)

	case *ast.SliceExpr:
		collectParamNamesExpr(v.Expr, seen)
		collectParamNamesExpr(v.From, seen)
		collectParamNamesExpr(v.To, seen)

	case *ast.LabelPredicate:
		collectParamNamesExpr(v.Receiver, seen)

	case *ast.ExistsSubquery:
		collectParamNamesPattern(v.Pattern, seen)
		if v.Where != nil {
			collectParamNamesExpr(v.Where.Predicate, seen)
		}
		if v.Query != nil {
			collectParamNamesSingle(v.Query, seen)
		}

	case *ast.CountSubquery:
		collectParamNamesPattern(v.Pattern, seen)
		if v.Query != nil {
			collectParamNamesSingle(v.Query, seen)
		}

	// Leaves: literals and variables carry no parameter references.
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.NullLiteral, *ast.Variable,
		*ast.OverflowIntLit:
		// nothing
	}
}

// sortStrings is a portable ascending sort for a string slice.
func sortStrings(s []string) {
	// insertion sort is fine for the small param-name sets expected in practice.
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j] > key {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
}
