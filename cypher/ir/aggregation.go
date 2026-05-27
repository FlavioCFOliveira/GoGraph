package ir

import (
	"fmt"
	"strings"

	"gograph/cypher/ast"
)

// aggregation.go — aggregate-function detection for RETURN / WITH projections.
//
// detectAggregation scans a Projection's item list and separates items into:
//   - groupBy: the non-aggregate projection expressions (grouping keys).
//   - aggs:    the aggregate descriptors for aggregate-function items.
//
// hasAgg is true when at least one aggregate function was found. The caller
// (translateWith, translateReturn) uses hasAgg to decide whether to wrap the
// child in an EagerAggregation operator.
//
// Recognised aggregate function names (case-insensitive, no namespace prefix):
//
//	count, sum, avg, min, max, collect
//
// count(*) is recognised when the argument list is empty or contains a single
// star literal (represented as the string "*").

// aggFunctions is the set of aggregate function names that trigger aggregation.
//
// All names are compared case-insensitively via [isAggregateFunc]. The set
// covers the openCypher TCK built-ins; missing entries cause the function to
// be treated as a scalar — which produces wrong results because the planner
// then refuses to emit an EagerAggregation.
var aggFunctions = map[string]bool{
	"count":          true,
	"sum":            true,
	"avg":            true,
	"min":            true,
	"max":            true,
	"collect":        true,
	"stdev":          true,
	"stdevp":         true,
	"percentilecont": true,
	"percentiledisc": true,
}

// isAggregateFunc reports whether name (lower-cased, no namespace) is an
// aggregate function.
func isAggregateFunc(name string) bool {
	return aggFunctions[strings.ToLower(name)]
}

// detectAggregation inspects proj and returns the grouping keys, parsed
// grouping-key AST expressions (one entry per groupBy, nil where the key is a
// bare alias), aggregate descriptors, and whether any aggregates were found.
//
// A projection item that contains an aggregate function call ANYWHERE in
// its expression (top-level or nested inside arithmetic, function calls,
// etc.) triggers aggregation. Bare aggregates (`count(x)`) emit one
// AggregateExpr with the item's surface name (alias or function string).
// Nested aggregates (`$age + avg(x.age) - 1000`) are extracted into
// synthetic `__agg_<n>` columns and the projection item is NOT a
// grouping key (it consumes one or more aggregates plus literals /
// parameters). The wrapping expression itself is preserved on
// ProjectionItem.Expr (via the rewritten form returned by
// [rewriteProjectionForAggregation]); the physical Projection evaluator
// resolves the synthetic Variable references against the
// EagerAggregation output row.
func detectAggregation(proj *ast.Projection) (
	groupBy []string,
	groupByExprs []ast.Expression,
	aggs []AggregateExpr,
	hasAgg bool,
) {
	if proj == nil {
		return nil, nil, nil, false
	}

	for _, item := range proj.Items {
		if fn, ok := extractAggFunc(item.Expr); ok {
			hasAgg = true
			outName := fn.String()
			if item.Alias != nil {
				outName = *item.Alias
			}
			argStr := ""
			var argExpr ast.Expression
			var secondArg ast.Expression
			if !fn.CountStar && len(fn.Args) >= 1 {
				argStr = fn.Args[0].String()
				if argStr == "*" {
					argStr = ""
				} else {
					argExpr = fn.Args[0]
				}
			}
			if !fn.CountStar && len(fn.Args) >= 2 {
				secondArg = fn.Args[1]
			}
			aggs = append(aggs, AggregateExpr{
				OutputName:    outName,
				Function:      strings.ToLower(fn.Name),
				Argument:      argStr,
				ArgumentExpr:  argExpr,
				SecondArgExpr: secondArg,
				Distinct:      fn.Distinct,
			})
			continue
		}

		if containsAggregate(item.Expr) {
			// Nested aggregate(s) — extract them; the wrapping expression
			// remains on the projection item (handled by the caller via
			// rewriteProjectionForAggregation). For grouping purposes the
			// item is NOT a key.
			hasAgg = true
			counter := len(aggs)
			_ = extractAggregatesFromExpr(item.Expr, &aggs, &counter)
			continue
		}

		// Pure non-aggregate item — becomes a grouping key.
		key := item.Expr.String()
		if item.Alias != nil {
			key = *item.Alias
		} else if v, ok := item.Expr.(*ast.Variable); ok {
			key = v.Name
		}
		groupBy = append(groupBy, key)
		groupByExprs = append(groupByExprs, item.Expr)
	}

	return groupBy, groupByExprs, aggs, hasAgg
}

// rewriteProjectionForAggregation walks proj.Items and, for any item whose
// expression contains a nested aggregate (not a bare aggregate at top level),
// returns a copy of the items list with the aggregate FunctionInvocations
// replaced by Variable references to synthetic `__agg_<n>` columns. The
// synthetic counter starts at the total aggregate count emitted by
// detectAggregation for all PREVIOUS items, mirroring detectAggregation's
// own bookkeeping so the synthetic Variable names line up with the
// EagerAggregation output schema. Returns nil if no rewriting was needed
// (caller can use the original items unchanged).
func rewriteProjectionForAggregation(proj *ast.Projection) []ProjectionItem {
	if proj == nil {
		return nil
	}
	out := make([]ProjectionItem, 0, len(proj.Items))
	aggCount := 0
	anyRewrite := false
	for _, item := range proj.Items {
		if _, ok := extractAggFunc(item.Expr); ok {
			out = append(out, ProjectionItem{
				Name:       projectionColumnName(item),
				Expression: item.Expr.String(),
				Expr:       item.Expr,
			})
			aggCount++
			continue
		}
		if !containsAggregate(item.Expr) {
			out = append(out, ProjectionItem{
				Name:       projectionColumnName(item),
				Expression: item.Expr.String(),
				Expr:       item.Expr,
			})
			continue
		}
		anyRewrite = true
		counter := aggCount
		var throwaway []AggregateExpr
		rewritten := extractAggregatesFromExpr(item.Expr, &throwaway, &counter)
		aggCount = counter
		out = append(out, ProjectionItem{
			Name:       projectionColumnName(item),
			Expression: rewritten.String(),
			Expr:       rewritten,
		})
	}
	if !anyRewrite {
		return nil
	}
	return out
}

// extractAggFunc returns the FunctionInvocation if expr is (or wraps) an
// aggregate function call with no namespace, otherwise returns nil, false.
func extractAggFunc(expr ast.Expression) (*ast.FunctionInvocation, bool) {
	fn, ok := expr.(*ast.FunctionInvocation)
	if !ok {
		return nil, false
	}
	// Namespace-qualified calls (e.g. apoc.agg.sum) are not treated as
	// built-in aggregates.
	if len(fn.Namespace) > 0 {
		return nil, false
	}
	if !isAggregateFunc(fn.Name) {
		return nil, false
	}
	return fn, true
}

// containsAggregate reports whether e or any of its sub-expressions contains
// a call to a built-in aggregate function (count/sum/avg/min/max/collect/
// stdev/stdevp/percentileCont/percentileDisc). Used to decide whether a
// projection item that is not itself a bare aggregate call should still
// trigger EagerAggregation insertion.
func containsAggregate(e ast.Expression) bool { //nolint:gocyclo // per-AST-node dispatch
	if e == nil {
		return false
	}
	switch n := e.(type) {
	case *ast.FunctionInvocation:
		if _, ok := extractAggFunc(n); ok {
			return true
		}
		for _, a := range n.Args {
			if containsAggregate(a) {
				return true
			}
		}
	case *ast.BinaryOp:
		return containsAggregate(n.Left) || containsAggregate(n.Right)
	case *ast.UnaryOp:
		return containsAggregate(n.Operand)
	case *ast.Property:
		return containsAggregate(n.Receiver)
	case *ast.SubscriptExpr:
		return containsAggregate(n.Expr) || containsAggregate(n.Index)
	case *ast.SliceExpr:
		return containsAggregate(n.Expr) || containsAggregate(n.From) || containsAggregate(n.To)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if containsAggregate(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, v := range n.Values {
			if containsAggregate(v) {
				return true
			}
		}
	case *ast.CaseExpression:
		if containsAggregate(n.Subject) || containsAggregate(n.ElseExpr) {
			return true
		}
		for _, alt := range n.Alternatives {
			if containsAggregate(alt.Condition) || containsAggregate(alt.Consequent) {
				return true
			}
		}
	case *ast.ListComprehension:
		// The Source expression sits in the outer scope — its
		// aggregates DO count toward the surrounding projection. The
		// Predicate/Projection run per element with v.Variable in
		// scope and are not eligible to introduce outer aggregates,
		// but a stray aggregate inside still surfaces the same
		// outer-scope rules; descend conservatively.
		if containsAggregate(n.Source) || containsAggregate(n.Predicate) || containsAggregate(n.Projection) {
			return true
		}
	}
	return false
}

// extractAggregatesFromExpr walks e and replaces every aggregate
// FunctionInvocation with a fresh Variable reference. The returned
// rewritten expression evaluates against the EagerAggregation output row
// (which carries one column per registered aggregate at a synthetic
// name). Each extracted aggregate is appended to *aggs as an
// AggregateExpr whose OutputName is the synthetic Variable name.
//
// counter is incremented per extracted aggregate to make names unique
// across the whole projection.
func extractAggregatesFromExpr(e ast.Expression, aggs *[]AggregateExpr, counter *int) ast.Expression { //nolint:gocyclo // per-AST-node dispatch
	if e == nil {
		return nil
	}
	if fn, ok := extractAggFunc(e); ok {
		name := fmt.Sprintf("__agg_%d", *counter)
		*counter++
		var argStr string
		var argExpr, secondArg ast.Expression
		if !fn.CountStar && len(fn.Args) >= 1 {
			argStr = fn.Args[0].String()
			if argStr != "*" {
				argExpr = fn.Args[0]
			} else {
				argStr = ""
			}
		}
		if !fn.CountStar && len(fn.Args) >= 2 {
			secondArg = fn.Args[1]
		}
		*aggs = append(*aggs, AggregateExpr{
			OutputName:    name,
			Function:      strings.ToLower(fn.Name),
			Argument:      argStr,
			ArgumentExpr:  argExpr,
			SecondArgExpr: secondArg,
			Distinct:      fn.Distinct,
		})
		return &ast.Variable{Name: name}
	}
	switch n := e.(type) {
	case *ast.BinaryOp:
		cp := *n
		cp.Left = extractAggregatesFromExpr(n.Left, aggs, counter)
		cp.Right = extractAggregatesFromExpr(n.Right, aggs, counter)
		return &cp
	case *ast.UnaryOp:
		cp := *n
		cp.Operand = extractAggregatesFromExpr(n.Operand, aggs, counter)
		return &cp
	case *ast.Property:
		cp := *n
		cp.Receiver = extractAggregatesFromExpr(n.Receiver, aggs, counter)
		return &cp
	case *ast.SubscriptExpr:
		cp := *n
		cp.Expr = extractAggregatesFromExpr(n.Expr, aggs, counter)
		cp.Index = extractAggregatesFromExpr(n.Index, aggs, counter)
		return &cp
	case *ast.FunctionInvocation:
		cp := *n
		cp.Args = make([]ast.Expression, len(n.Args))
		for i, a := range n.Args {
			cp.Args[i] = extractAggregatesFromExpr(a, aggs, counter)
		}
		return &cp
	case *ast.ListComprehension:
		cp := *n
		cp.Source = extractAggregatesFromExpr(n.Source, aggs, counter)
		// Predicate/Projection run per-element after the Source has
		// been collected; we still pass through the walker so any
		// (uncommon) aggregate is registered. The same conservative
		// stance as containsAggregate.
		cp.Predicate = extractAggregatesFromExpr(n.Predicate, aggs, counter)
		cp.Projection = extractAggregatesFromExpr(n.Projection, aggs, counter)
		return &cp
	case *ast.MapLiteral:
		cp := *n
		cp.Values = make([]ast.Expression, len(n.Values))
		for i, v := range n.Values {
			cp.Values[i] = extractAggregatesFromExpr(v, aggs, counter)
		}
		return &cp
	case *ast.ListLiteral:
		cp := *n
		cp.Elements = make([]ast.Expression, len(n.Elements))
		for i, el := range n.Elements {
			cp.Elements[i] = extractAggregatesFromExpr(el, aggs, counter)
		}
		return &cp
	case *ast.SliceExpr:
		cp := *n
		cp.Expr = extractAggregatesFromExpr(n.Expr, aggs, counter)
		cp.From = extractAggregatesFromExpr(n.From, aggs, counter)
		cp.To = extractAggregatesFromExpr(n.To, aggs, counter)
		return &cp
	case *ast.CaseExpression:
		cp := *n
		cp.Subject = extractAggregatesFromExpr(n.Subject, aggs, counter)
		cp.ElseExpr = extractAggregatesFromExpr(n.ElseExpr, aggs, counter)
		cp.Alternatives = make([]*ast.CaseAlternative, len(n.Alternatives))
		for i, alt := range n.Alternatives {
			altCp := *alt
			altCp.Condition = extractAggregatesFromExpr(alt.Condition, aggs, counter)
			altCp.Consequent = extractAggregatesFromExpr(alt.Consequent, aggs, counter)
			cp.Alternatives[i] = &altCp
		}
		return &cp
	}
	return e
}
