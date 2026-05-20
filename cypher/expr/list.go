package expr

// list.go — list-specific expression evaluation (task-264).
//
// Implements:
//   - List slicing: list[from..to] (ast.SliceExpr), half-open range [from, to).
//   - List comprehension: [x IN list WHERE pred | transform] (ast.ListComprehension).
//
// List indexing (list[n]) and list concatenation (list1 + list2) are handled
// in eval.go (evalSubscript and evalArith respectively) and are not duplicated
// here.
//
// NULL propagation:
//   - Slicing: if the source list is NULL, return NULL. NULL bounds are treated
//     as absent (start-of-list or end-of-list respectively).
//   - Comprehension: if the source is NULL, return an empty list (openCypher
//     semantics: iterating over NULL yields no rows).

import "gograph/cypher/ast"

// evalSlice evaluates a [ast.SliceExpr]: expr[from..to].
//
// Semantics:
//   - from defaults to 0 when nil or NULL.
//   - to defaults to len(list) when nil or NULL.
//   - Negative bounds are resolved relative to the end of the list.
//   - Out-of-range bounds are clamped to [0, len(list)].
//   - The result is the half-open slice [from, to).
func evalSlice(n *ast.SliceExpr, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	src, err := evalExpr(n.Expr, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(src) {
		return Null, nil
	}
	list, ok := src.(ListValue)
	if !ok {
		// Slicing on a non-list → NULL per openCypher.
		return Null, nil
	}
	ln := len(list)

	from := 0
	if n.From != nil {
		fv, err := evalExpr(n.From, row, params, reg)
		if err != nil {
			return nil, err
		}
		if !IsNull(fv) {
			iv, ok := fv.(IntegerValue)
			if !ok {
				return Null, nil
			}
			from = resolveIndex(int(iv), ln)
		}
	}

	to := ln
	if n.To != nil {
		tv, err := evalExpr(n.To, row, params, reg)
		if err != nil {
			return nil, err
		}
		if !IsNull(tv) {
			iv, ok := tv.(IntegerValue)
			if !ok {
				return Null, nil
			}
			// Slice upper bound: do not offset-wrap negative values (openCypher
			// slice upper bounds are positional, not from-end).
			to = int(iv)
			if to < 0 {
				to = 0
			}
			if to > ln {
				to = ln
			}
		}
	}

	if from > to {
		from = to
	}
	result := make(ListValue, to-from)
	copy(result, list[from:to])
	return result, nil
}

// resolveIndex resolves a list index, handling negative indices (from end).
// The returned index is clamped to [0, length].
func resolveIndex(idx, length int) int {
	if idx < 0 {
		idx = length + idx
	}
	if idx < 0 {
		idx = 0
	}
	if idx > length {
		idx = length
	}
	return idx
}

// evalListComprehension evaluates [variable IN source WHERE predicate | projection].
//
// If predicate is nil, all elements pass. If projection is nil, the element
// itself is the output. A NULL source is treated as an empty list.
func evalListComprehension(n *ast.ListComprehension, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	src, err := evalExpr(n.Source, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(src) {
		return ListValue{}, nil
	}
	list, ok := src.(ListValue)
	if !ok {
		return ListValue{}, nil
	}

	result := make(ListValue, 0, len(list))
	for _, elem := range list {
		// Bind the loop variable.
		innerRow := make(RowContext, len(row)+1)
		for k, v := range row {
			innerRow[k] = v
		}
		innerRow[n.Variable] = elem

		// Apply WHERE predicate if present.
		if n.Predicate != nil {
			pv, err := evalExpr(n.Predicate, innerRow, params, reg)
			if err != nil {
				return nil, err
			}
			if !IsTruthy(pv) {
				continue
			}
		}

		// Apply projection if present, otherwise use the element as-is.
		if n.Projection != nil {
			out, err := evalExpr(n.Projection, innerRow, params, reg)
			if err != nil {
				return nil, err
			}
			result = append(result, out)
		} else {
			result = append(result, elem)
		}
	}
	return result, nil
}
