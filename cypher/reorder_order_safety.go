package cypher

// reorder_order_safety.go — the order-safety suppression predicate for the
// count-store-gated join-reorder peepholes (task #2092, docs/reordering-
// design.md §4; full operator taxonomy in the cypher-expert-consultant memory
// "spec-join-reorder-order-safety").
//
// A reorder permutes the row-emission order of a bag. openCypher leaves result
// order unspecified absent an order-establishing operator (openCypher 9, "Order
// of results"; CIP2016-06-14). The TCK compares unordered results as a BAG and
// ordered results (explicit ORDER BY, or a procedure YIELD) as an ordered LIST.
// So permuting an unordered result is conformant, but permuting a stream that a
// row-position-observing operator consumes is NOT.
//
// [SuppressReorder] answers, for a given reorder point, whether any operator on
// the spine from that point up to the root would observe the changed order. It
// walks the spine NEAREST-ANCESTOR FIRST; the first decisive operator wins:
//
//   - RESET (enabler): a TOTAL Sort/Top erases arrival order, so everything
//     above it is order-blind → return false (safe) immediately. Totality is
//     required: a non-total sort leaves tie-groups in arrival order.
//     [isTotalOrderSort] defaults to NOT total unless totality is proven; under-
//     conservatism here would break the TCK's 86 in-order scenarios, whereas
//     over-conservatism only forfeits an optimisation.
//   - SUPPRESS (observer): a bare Limit/Skip (no dominating total sort — it
//     changes WHICH rows survive, a multiset change), an EagerAggregation or
//     Projection carrying collect() (a value trap: an ordered list built in
//     arrival order), a RollUpApply (pattern comprehension), a ProcedureCall
//     (yielded order), an Unwind (conservative), or a non-total Sort/Top →
//     return true (suppress).
//   - NEUTRAL: Selection, the Expand family, the Apply family, Distinct,
//     order-blind aggregations, Union/UnionAll, existence/count subqueries,
//     scalar Projection/WITH, and the plan root pass through unchanged.
//
// Reaching the root with no observer means the result order is unspecified →
// return false (safe). Any operator not positively classified as neutral or as
// a RESET enabler is treated as an observer (fail safe): "when in doubt keep the
// deterministic plan — never wrong, only slower."
//
// This predicate is intentionally a SUPERSET of the shipped hash-join order
// check [hashJoinOrderSafe] (which inspects only Limit/Skip and EagerAggregation
// over the whole plan): it suppresses in strictly more cases, so it cannot admit
// a reorder the hash-join check would have rejected. The shipped hash join is
// left untouched.

import (
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// SuppressReorder reports whether a reorder at some point in the logical plan
// must be suppressed because an operator on its spine observes row order. The
// spine is the chain of ancestors of the reorder point, NEAREST ANCESTOR FIRST:
// spine[0] is the reorder point's immediate parent and spine[len-1] is the plan
// root. It returns true to suppress (keep the written order) and false when the
// reorder is order-safe. An empty spine (reorder at the root) is safe.
func SuppressReorder(spine []ir.LogicalPlan) bool {
	for _, p := range spine {
		switch n := p.(type) {
		case *ir.Sort:
			// A total sort re-establishes a complete order, masking every
			// permutation below it; a non-total sort leaves tie-groups in arrival
			// order and therefore observes the reorder.
			if isTotalOrderSort(n.SortItems, n) {
				return false
			}
			return true
		case *ir.Top:
			if isTotalOrderSort(n.SortItems, n) {
				return false
			}
			return true
		case *ir.Limit, *ir.Skip:
			// A bare LIMIT/SKIP reached before any dominating total sort selects
			// WHICH rows survive — a multiset change, not merely a reorder.
			return true
		case *ir.EagerAggregation:
			if aggregationSuppressesReorder(n) {
				return true
			}
			// Order-blind aggregation: neutral, keep walking.
		case *ir.Projection:
			if projectionSuppressesReorder(n) {
				return true
			}
			// Scalar projection / WITH: neutral, keep walking.
		case *ir.RollUpApply:
			// Pattern comprehension: the collected list reflects arrival order.
			return true
		case *ir.ProcedureCall:
			// A procedure's YIELD order is observable (TCK "in order" for yields).
			return true
		case *ir.Unwind:
			// Conservative: UNWIND fans a row out into list order, and its own
			// input order is then carried into that expansion.
			return true
		case *ir.Selection,
			*ir.Distinct,
			*ir.Apply, *ir.CorrelatedApply, *ir.OptionalApply,
			*ir.SemiApply, *ir.AntiSemiApply,
			*ir.Expand, *ir.OptionalExpand, *ir.VarLengthExpand,
			*ir.ShortestPath, *ir.ProjectEndpoints, *ir.NamedPath,
			*ir.Union, *ir.UnionAll,
			*ir.SubqueryExists, *ir.SubqueryCount,
			*ir.Eager, *ir.ProduceResults:
			// Neutral: none of these observes the arrival order of the reordered
			// bag (aggregations are order-blind here; set operators compare as
			// bags/sets; subqueries open their own scope).
		default:
			// Any operator not proven order-transparent (write operators, FOREACH,
			// future node types) suppresses conservatively.
			return true
		}
	}
	return false
}

// isTotalOrderSort reports whether a sort's key list establishes a TOTAL order
// over the rows flowing through it — i.e. no two distinct rows can tie. Only a
// total sort is a reorder RESET enabler; a non-total sort leaves tie-groups in
// arrival order, which a reorder would perturb.
//
// Totality defaults to FALSE and is asserted only when provable from the plan
// alone. The provable, sound condition is that EVERY column flowing through the
// sort is PINNED by a sort key that is injective in that column: the bare column
// variable, or id(v) / elementId(v) over it. When every column is pinned, two
// rows with an equal key tuple are equal in every column — identical rows, whose
// relative order is unobservable. Constraint-backed uniqueness (a UNIQUE /
// NODE KEY property) would also prove totality but needs the schema, which is
// not available at this layer, so it is intentionally not asserted here (a
// missed optimisation, never an unsafe reorder).
func isTotalOrderSort(items []ir.SortItem, node ir.LogicalPlan) bool {
	if len(items) == 0 {
		return false
	}
	// The full column set flowing through the sort. collectPlanVars descends the
	// whole subtree because several operators report only the variables they
	// introduce in Vars(); an over-count only makes this stricter (a spurious
	// column that no key pins yields "not total").
	cols := collectPlanVars(node)
	if len(cols) == 0 {
		return false
	}
	pinned := make(map[string]struct{}, len(items))
	for _, it := range items {
		if v, ok := sortKeyPinnedVar(it.Expr); ok {
			pinned[v] = struct{}{}
		}
	}
	for c := range cols {
		if _, ok := pinned[c]; !ok {
			return false
		}
	}
	return true
}

// sortKeyPinnedVar returns the single column variable a sort key pins injectively
// — the bare variable v, or id(v) / elementId(v) — or ("", false) for any other
// key (a property access, arithmetic, a literal, a multi-argument call). id and
// elementId are injective over entities, and orderability orders a bare entity
// by that same identity, so each such key equals across two rows only when the
// underlying column is equal.
func sortKeyPinnedVar(e ast.Expression) (string, bool) {
	switch n := e.(type) {
	case *ast.Variable:
		return n.Name, true
	case *ast.FunctionInvocation:
		if len(n.Namespace) != 0 || n.Distinct || n.CountStar || len(n.Args) != 1 {
			return "", false
		}
		switch strings.ToLower(n.Name) {
		case "id", "elementid":
			if v, ok := n.Args[0].(*ast.Variable); ok {
				return v.Name, true
			}
		}
	}
	return "", false
}

// aggregationSuppressesReorder reports whether an EagerAggregation carries an
// aggregate whose result depends on the arrival order of its input rows. collect
// (and collect(DISTINCT), whose list keeps first-occurrence order) is the trap;
// count / sum / avg / min / max / stDev / stDevP / percentileCont / percentileDisc
// are commutative-associative or range over orderability, never arrival order.
// Any unrecognised aggregate is treated conservatively as order-observing.
func aggregationSuppressesReorder(agg *ir.EagerAggregation) bool {
	for _, a := range agg.Aggregates {
		if isOrderSensitiveAggregate(a.Function) {
			return true
		}
	}
	return false
}

// isOrderSensitiveAggregate reports whether an aggregate function name denotes an
// arrival-order-dependent aggregate. The order-BLIND set is fixed by the
// openCypher aggregation semantics; everything else (collect, and any unknown
// aggregate) is order-sensitive.
func isOrderSensitiveAggregate(name string) bool {
	switch normaliseAggName(name) {
	case "count", "sum", "avg", "min", "max", "stdev", "stdevp",
		"percentilecont", "percentiledisc":
		return false
	default:
		return true
	}
}

// projectionSuppressesReorder reports whether any projection item embeds an
// order-sensitive construct — collect(), reduce() (a potentially non-commutative
// fold such as string concatenation), or a pattern comprehension. In this engine
// aggregations are lifted into an EagerAggregation, so a raw collect() rarely
// survives in a projection item; this scan is defence in depth over that
// invariant and never fires for a scalar projection (RETURN a, b).
func projectionSuppressesReorder(proj *ir.Projection) bool {
	for _, it := range proj.Items {
		if it.Expr != nil && exprHasOrderSensitiveConstruct(it.Expr) {
			return true
		}
	}
	return false
}

// exprHasOrderSensitiveConstruct reports whether an expression tree contains a
// construct whose value depends on the arrival order of the rows feeding it: a
// collect() call, a reduce() fold, or a pattern comprehension. It walks all
// order-relevant sub-expression positions; nodes that cannot host such a
// construct (variables, parameters, literals) contribute nothing.
func exprHasOrderSensitiveConstruct(root ast.Expression) bool {
	found := false
	var walk func(ast.Expression)
	walk = func(e ast.Expression) {
		if found || e == nil {
			return
		}
		switch n := e.(type) {
		case *ast.FunctionInvocation:
			if len(n.Namespace) == 0 && normaliseAggName(n.Name) == "collect" {
				found = true
				return
			}
			for _, a := range n.Args {
				walk(a)
			}
		case *ast.ReduceExpr:
			found = true
		case *ast.PatternComprehension:
			found = true
		case *ast.Property:
			walk(n.Receiver)
		case *ast.BinaryOp:
			walk(n.Left)
			walk(n.Right)
		case *ast.UnaryOp:
			walk(n.Operand)
		case *ast.LabelPredicate:
			walk(n.Receiver)
		case *ast.CaseExpression:
			walk(n.Subject)
			for _, alt := range n.Alternatives {
				if alt != nil {
					walk(alt.Condition)
					walk(alt.Consequent)
				}
			}
			walk(n.ElseExpr)
		case *ast.ListComprehension:
			walk(n.Source)
			walk(n.Predicate)
			walk(n.Projection)
		case *ast.MapProjection:
			walk(n.Subject)
			for _, it := range n.Items {
				if it != nil {
					walk(it.Value)
				}
			}
		case *ast.ListLiteral:
			for _, el := range n.Elements {
				walk(el)
			}
		case *ast.MapLiteral:
			for _, v := range n.Values {
				walk(v)
			}
		case *ast.SubscriptExpr:
			walk(n.Expr)
			walk(n.Index)
		case *ast.SliceExpr:
			walk(n.Expr)
			walk(n.From)
			walk(n.To)
		default:
			// Variable, Parameter, literals, EXISTS/COUNT subqueries: no embedded
			// order-sensitive construct reachable here.
		}
	}
	walk(root)
	return found
}
