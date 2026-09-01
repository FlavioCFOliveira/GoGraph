package ir

// nondeterministic.go — the deny-set of scalar functions whose result is not
// (pure ∧ row-local).
//
// It lives here, in the IR package, because it has TWO consumers that must never
// disagree:
//
//   - the physical builder's morsel-parallel fused scan (#1682), which must not
//     evaluate such a call independently per worker; and
//   - [applyProjectionTail]'s ORDER BY … SKIP … LIMIT fusion (#2509), which must
//     not emit Skip(s) over Top(s+k) when s or k would be evaluated twice — once
//     for the fused bound and once for the Skip — and could yield two different
//     numbers.
//
// A second copy of the set in the planner would be a second place to forget a
// function name, so the set is defined once and exported for the cypher package
// to reach.

import (
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// nonDeterministicFuncs is the deny-set. Two evaluations of the same call can
// yield different values.
//
// Two groups (cypher-expert sign-off):
//
//   - rand / randomUUID: non-deterministic per call;
//   - the zero-argument "clock" temporal constructors: even though GoGraph freezes
//     one per-query "now" in the shared registry (newNowAwareRegistry) so two
//     evaluations would in fact observe the same instant, they are rejected as
//     cheap, unambiguous insurance — these queries are rare and the safety margin
//     is worth more than the optimisation. Argument-bearing temporal forms
//     (date('2020-01-01'), datetime(n.ts)) are pure and are NOT in this set.
//
// Names are matched case-insensitively against the call's bare function name; the
// no-namespace assumption matches the built-in registry's flat namespace.
var nonDeterministicFuncs = map[string]struct{}{
	"rand":          {},
	"randomuuid":    {},
	"timestamp":     {},
	"date":          {},
	"datetime":      {},
	"localdatetime": {},
	"time":          {},
	"localtime":     {},
}

// IsNonDeterministicCall reports whether fn is a call the engine must not
// evaluate more than once and expect the same answer: rand / randomUUID (always
// non-deterministic), or a ZERO-ARGUMENT temporal clock constructor (date(),
// datetime(), localdatetime(), time(), localtime(), timestamp()). The temporal
// names are only non-deterministic in their no-argument clock form; an
// argument-bearing call (date('2020-01-01'), datetime(n.ts)) is pure and is not
// rejected. rand / randomUUID are rejected regardless of arguments.
func IsNonDeterministicCall(fn *ast.FunctionInvocation) bool {
	if fn == nil {
		return false
	}
	if len(fn.Namespace) != 0 {
		return false // namespaced calls (apoc.*, …) are not the built-in clock/rand forms
	}
	name := strings.ToLower(fn.Name)
	if _, ok := nonDeterministicFuncs[name]; !ok {
		return false
	}
	switch name {
	case "rand", "randomuuid":
		return true // non-deterministic for any argument shape
	default:
		// Temporal constructor: only the zero-argument clock form is rejected.
		return len(fn.Args) == 0
	}
}

// ExprIsDeterministic reports whether every function call reachable from e is
// safe to evaluate twice. It is the gate [applyProjectionTail] applies to a SKIP
// or LIMIT expression before fusing, because the fused shape resolves the offset
// once for the Top bound and once for the Skip above it.
//
// It walks the same AST node set [collectOrderByVars] does. An unrecognised node
// type is treated as DETERMINISTIC only when it carries no sub-expression the
// walk can miss; the switch below therefore enumerates every composite node the
// expression grammar admits, and the default arm covers the leaves (literals,
// variables, parameters), which contain no call.
func ExprIsDeterministic(e ast.Expression) bool {
	switch n := e.(type) {
	case nil:
		return true
	case *ast.FunctionInvocation:
		if IsNonDeterministicCall(n) {
			return false
		}
		for _, a := range n.Args {
			if !ExprIsDeterministic(a) {
				return false
			}
		}
	case *ast.BinaryOp:
		return ExprIsDeterministic(n.Left) && ExprIsDeterministic(n.Right)
	case *ast.UnaryOp:
		return ExprIsDeterministic(n.Operand)
	case *ast.Property:
		return ExprIsDeterministic(n.Receiver)
	case *ast.LabelPredicate:
		return ExprIsDeterministic(n.Receiver)
	case *ast.SubscriptExpr:
		return ExprIsDeterministic(n.Expr) && ExprIsDeterministic(n.Index)
	case *ast.SliceExpr:
		return ExprIsDeterministic(n.Expr) && ExprIsDeterministic(n.From) && ExprIsDeterministic(n.To)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if !ExprIsDeterministic(el) {
				return false
			}
		}
	case *ast.MapLiteral:
		for _, v := range n.Values {
			if !ExprIsDeterministic(v) {
				return false
			}
		}
	case *ast.CaseExpression:
		if !ExprIsDeterministic(n.Subject) || !ExprIsDeterministic(n.ElseExpr) {
			return false
		}
		for _, alt := range n.Alternatives {
			if !ExprIsDeterministic(alt.Condition) || !ExprIsDeterministic(alt.Consequent) {
				return false
			}
		}
	case *ast.ListComprehension, *ast.PatternComprehension, *ast.ReduceExpr,
		*ast.ExistsSubquery, *ast.CountSubquery:
		// These evaluate a sub-expression per element or run a sub-plan. A SKIP
		// or LIMIT is never written this way in practice, and proving twice-safety
		// for them is not worth the surface, so they refuse the fusion.
		return false
	}
	return true
}
