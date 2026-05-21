package ir

import (
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
	"count":   true,
	"sum":     true,
	"avg":     true,
	"min":     true,
	"max":     true,
	"collect": true,
	"stdev":   true,
	"stdevp":  true,
}

// isAggregateFunc reports whether name (lower-cased, no namespace) is an
// aggregate function.
func isAggregateFunc(name string) bool {
	return aggFunctions[strings.ToLower(name)]
}

// detectAggregation inspects proj and returns the grouping keys, parsed
// grouping-key AST expressions (one entry per groupBy, nil where the key is a
// bare alias), aggregate descriptors, and whether any aggregates were found.
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
		fn, ok := extractAggFunc(item.Expr)
		if !ok {
			// Non-aggregate item — becomes a grouping key.
			// Use alias if present, otherwise the expression string.
			key := item.Expr.String()
			if item.Alias != nil {
				key = *item.Alias
			} else if v, ok := item.Expr.(*ast.Variable); ok {
				key = v.Name
			}
			groupBy = append(groupBy, key)
			groupByExprs = append(groupByExprs, item.Expr)
			continue
		}

		hasAgg = true

		// Determine the output name.
		outName := fn.String()
		if item.Alias != nil {
			outName = *item.Alias
		}

		// Build the argument string and capture the parsed argument expression.
		// count(*) has no args.
		argStr := ""
		var argExpr ast.Expression
		if len(fn.Args) == 1 {
			argStr = fn.Args[0].String()
			if argStr == "*" {
				argStr = "" // normalise count(*) → Argument=""
			} else {
				argExpr = fn.Args[0]
			}
		}

		aggs = append(aggs, AggregateExpr{
			OutputName:   outName,
			Function:     strings.ToLower(fn.Name),
			Argument:     argStr,
			ArgumentExpr: argExpr,
			Distinct:     fn.Distinct,
		})
	}

	return groupBy, groupByExprs, aggs, hasAgg
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
