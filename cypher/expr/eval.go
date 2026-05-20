package expr

// eval.go — Cypher expression evaluator with three-valued logic (task-247).
//
// Eval dispatches on the concrete AST node type and recursively evaluates
// sub-expressions. The implementation follows openCypher 9 semantics:
//
//   - Comparisons involving NULL return NULL (3VL).
//   - IS NULL / IS NOT NULL always return a Bool.
//   - AND/OR/NOT follow the Kleene three-valued truth tables.
//   - Arithmetic promotes Int+Float→Float; Int+Int→Int; Float+Float→Float.
//
// # Concurrency
//
// Eval is stateless and safe for concurrent use.

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"gograph/cypher/ast"
)

// RowContext maps variable names to their current runtime values.
// It is typically derived from the current operator row plus the schema
// mapping maintained by the executor.
type RowContext map[string]Value

// FunctionRegistry resolves built-in and user-defined functions by name.
// Implementations must be safe for concurrent use (read-only after init).
type FunctionRegistry interface {
	// Resolve returns the BuiltinFn for name, or (nil, false) when unknown.
	// name is lower-cased by the caller before lookup.
	Resolve(name string) (BuiltinFn, bool)
}

// BuiltinFn is the signature of a built-in Cypher function.
type BuiltinFn func(args []Value) (Value, error)

// EvalError is returned when Eval encounters a type or semantic error that
// is not representable as a NULL (e.g. unknown operator, unsupported AST node).
type EvalError struct {
	Msg string
}

func (e *EvalError) Error() string { return "eval: " + e.Msg }

// Eval evaluates expr in the context of row and params. It dispatches on the
// concrete AST node type and returns the resulting Value. An EvalError is
// returned for unsupported constructs; all other errors propagate from the
// function registry.
//
// If reg is nil, function invocations return an EvalError.
func Eval(expr ast.Expression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	return evalExpr(expr, row, params, reg)
}

func evalExpr(e ast.Expression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	switch n := e.(type) {
	// ── Literals ──────────────────────────────────────────────────────────────
	case *ast.NullLiteral:
		return Null, nil
	case *ast.BoolLiteral:
		return BoolValue(n.Value), nil
	case *ast.IntLiteral:
		return IntegerValue(n.Value), nil
	case *ast.FloatLiteral:
		return FloatValue(n.Value), nil
	case *ast.StringLiteral:
		return StringValue(n.Value), nil

	// ── Composite literals ─────────────────────────────────────────────────────
	case *ast.ListLiteral:
		return evalListLiteral(n, row, params, reg)
	case *ast.MapLiteral:
		return evalMapLiteral(n, row, params, reg)

	// ── Variable and parameter ─────────────────────────────────────────────────
	case *ast.Variable:
		if v, ok := row[n.Name]; ok {
			return v, nil
		}
		return Null, nil // unbound variable → NULL per openCypher semantics

	case *ast.Parameter:
		if params != nil {
			if v, ok := params[n.Name]; ok {
				return v, nil
			}
		}
		return Null, nil // unset parameter → NULL

	// ── Property access ────────────────────────────────────────────────────────
	case *ast.Property:
		return evalProperty(n, row, params, reg)

	// ── Subscript access ───────────────────────────────────────────────────────
	case *ast.SubscriptExpr:
		return evalSubscript(n, row, params, reg)

	// ── Binary operator ────────────────────────────────────────────────────────
	case *ast.BinaryOp:
		return evalBinaryOp(n, row, params, reg)

	// ── Unary operator ─────────────────────────────────────────────────────────
	case *ast.UnaryOp:
		return evalUnaryOp(n, row, params, reg)

	// ── CASE expression ────────────────────────────────────────────────────────
	case *ast.CaseExpression:
		return evalCase(n, row, params, reg)

	// ── Function call ──────────────────────────────────────────────────────────
	case *ast.FunctionInvocation:
		return evalFunction(n, row, params, reg)

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported expression type %T", e)}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Composite literals
// ─────────────────────────────────────────────────────────────────────────────

func evalListLiteral(n *ast.ListLiteral, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(ListValue, len(n.Elements))
	for i, elem := range n.Elements {
		v, err := evalExpr(elem, row, params, reg)
		if err != nil {
			return nil, err
		}
		result[i] = v
	}
	return result, nil
}

func evalMapLiteral(n *ast.MapLiteral, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(MapValue, len(n.Keys))
	for i, k := range n.Keys {
		v, err := evalExpr(n.Values[i], row, params, reg)
		if err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Property access
// ─────────────────────────────────────────────────────────────────────────────

func evalProperty(n *ast.Property, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	recv, err := evalExpr(n.Receiver, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(recv) {
		return Null, nil
	}
	switch r := recv.(type) {
	case NodeValue:
		if r.Properties != nil {
			if v, ok := r.Properties[n.Key]; ok {
				return v, nil
			}
		}
		return Null, nil
	case RelationshipValue:
		if r.Properties != nil {
			if v, ok := r.Properties[n.Key]; ok {
				return v, nil
			}
		}
		return Null, nil
	case MapValue:
		if v, ok := r[n.Key]; ok {
			return v, nil
		}
		return Null, nil
	default:
		// Property access on a non-map/node/rel returns NULL per openCypher.
		return Null, nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Subscript access
// ─────────────────────────────────────────────────────────────────────────────

func evalSubscript(n *ast.SubscriptExpr, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	container, err := evalExpr(n.Expr, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(container) {
		return Null, nil
	}
	idx, err := evalExpr(n.Index, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(idx) {
		return Null, nil
	}
	switch c := container.(type) {
	case ListValue:
		i, ok := idx.(IntegerValue)
		if !ok {
			return Null, nil
		}
		pos := int(i)
		if pos < 0 {
			pos = len(c) + pos
		}
		if pos < 0 || pos >= len(c) {
			return Null, nil
		}
		return c[pos], nil
	case MapValue:
		k, ok := idx.(StringValue)
		if !ok {
			return Null, nil
		}
		if v, exists := c[string(k)]; exists {
			return v, nil
		}
		return Null, nil
	default:
		return Null, nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Binary operator
// ─────────────────────────────────────────────────────────────────────────────

func evalBinaryOp(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	// AND and OR short-circuit under 3VL before evaluating right.
	switch n.Operator {
	case "AND":
		return eval3VLAND(n, row, params, reg)
	case "OR":
		return eval3VLOR(n, row, params, reg)
	}

	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}

	switch n.Operator {
	// ── Equality / inequality ─────────────────────────────────────────────────
	case "=":
		return left.Equal(right), nil
	case "<>":
		eq := left.Equal(right)
		if IsNull(eq) {
			return Null, nil
		}
		return BoolValue(!IsTruthy(eq)), nil

	// ── Ordering comparisons ──────────────────────────────────────────────────
	case "<", "<=", ">", ">=":
		return evalOrdering(n.Operator, left, right)

	// ── Arithmetic ────────────────────────────────────────────────────────────
	case "+":
		return evalArith("+", left, right)
	case "-":
		return evalArith("-", left, right)
	case "*":
		return evalArith("*", left, right)
	case "/":
		return evalArith("/", left, right)
	case "%":
		return evalArith("%", left, right)
	case "^":
		return evalArith("^", left, right)

	// ── String operators ──────────────────────────────────────────────────────
	case "CONTAINS":
		return evalStringOp("CONTAINS", left, right)
	case "STARTS WITH":
		return evalStringOp("STARTS WITH", left, right)
	case "ENDS WITH":
		return evalStringOp("ENDS WITH", left, right)
	case "=~":
		return evalStringOp("=~", left, right)

	// ── List / map membership ─────────────────────────────────────────────────
	case "IN":
		return evalIn(left, right)

	// ── XOR ──────────────────────────────────────────────────────────────────
	case "XOR":
		return eval3VLXOR(left, right)

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported binary operator %q", n.Operator)}
	}
}

// eval3VLAND evaluates AND with Kleene 3VL short-circuit.
func eval3VLAND(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false (short-circuit, even over NULL)
	if b, ok := left.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false
	if b, ok := right.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	// NULL AND NULL = NULL; NULL AND true = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(true), nil
}

// eval3VLOR evaluates OR with Kleene 3VL short-circuit.
func eval3VLOR(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true (short-circuit, even over NULL)
	if IsTruthy(left) {
		return BoolValue(true), nil
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true
	if IsTruthy(right) {
		return BoolValue(true), nil
	}
	// NULL OR false = NULL; NULL OR NULL = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(false), nil
}

// eval3VLXOR evaluates XOR with 3VL: NULL XOR _ = NULL.
func eval3VLXOR(left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	lb, lok := left.(BoolValue)
	rb, rok := right.(BoolValue)
	if !lok || !rok {
		return Null, nil
	}
	return BoolValue(bool(lb) != bool(rb)), nil
}

// evalOrdering handles <, <=, >, >= with 3VL: NULL operand → NULL.
func evalOrdering(op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	cmp, err := compareValues(left, right)
	if err != nil {
		return Null, nil //nolint:nilerr // type mismatch → NULL per openCypher
	}
	switch op {
	case "<":
		return BoolValue(cmp < 0), nil
	case "<=":
		return BoolValue(cmp <= 0), nil
	case ">":
		return BoolValue(cmp > 0), nil
	case ">=":
		return BoolValue(cmp >= 0), nil
	}
	return Null, nil
}

// compareValues compares two non-null values of compatible types.
// Returns an error when the types are incompatible for ordering.
func compareValues(a, b Value) (int, error) {
	// Promote Int to Float when comparing with Float.
	a, b = promoteNumeric(a, b)
	switch av := a.(type) {
	case IntegerValue:
		if bv, ok := b.(IntegerValue); ok {
			return cmpInt64(int64(av), int64(bv)), nil
		}
	case FloatValue:
		if bv, ok := b.(FloatValue); ok {
			return cmpFloat64(float64(av), float64(bv)), nil
		}
	case StringValue:
		if bv, ok := b.(StringValue); ok {
			s1, s2 := string(av), string(bv)
			if s1 < s2 {
				return -1, nil
			}
			if s1 > s2 {
				return 1, nil
			}
			return 0, nil
		}
	case BoolValue:
		if bv, ok := b.(BoolValue); ok {
			return compareBool(bool(av), bool(bv)), nil
		}
	}
	return 0, &EvalError{Msg: fmt.Sprintf("incompatible types for comparison: %s vs %s", a.Kind(), b.Kind())}
}

// promoteNumeric promotes Int/Float pairs so that arithmetic is consistent.
func promoteNumeric(a, b Value) (Value, Value) {
	_, aIsInt := a.(IntegerValue)
	_, bIsFloat := b.(FloatValue)
	if aIsInt && bIsFloat {
		return FloatValue(float64(a.(IntegerValue))), b //nolint:forcetypeassert // kind pre-checked
	}
	_, aIsFloat := a.(FloatValue)
	_, bIsInt := b.(IntegerValue)
	if aIsFloat && bIsInt {
		return a, FloatValue(float64(b.(IntegerValue))) //nolint:forcetypeassert // kind pre-checked
	}
	return a, b
}

// evalArith evaluates arithmetic binary operators.
func evalArith(op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	// String concatenation: "+" between strings.
	if op == "+" {
		if ls, lok := left.(StringValue); lok {
			if rs, rok := right.(StringValue); rok {
				return StringValue(string(ls) + string(rs)), nil
			}
		}
		// List concatenation.
		if ll, lok := left.(ListValue); lok {
			if rl, rok := right.(ListValue); rok {
				result := make(ListValue, len(ll)+len(rl))
				copy(result, ll)
				copy(result[len(ll):], rl)
				return result, nil
			}
		}
	}
	left, right = promoteNumeric(left, right)
	switch lv := left.(type) {
	case IntegerValue:
		rv, ok := right.(IntegerValue)
		if !ok {
			return Null, nil
		}
		return evalIntArith(op, int64(lv), int64(rv))
	case FloatValue:
		rv, ok := right.(FloatValue)
		if !ok {
			return Null, nil
		}
		return evalFloatArith(op, float64(lv), float64(rv))
	}
	return Null, nil
}

func evalIntArith(op string, a, b int64) (Value, error) {
	switch op {
	case "+":
		return IntegerValue(a + b), nil
	case "-":
		return IntegerValue(a - b), nil
	case "*":
		return IntegerValue(a * b), nil
	case "/":
		if b == 0 {
			return Null, nil // division by zero → NULL in Cypher
		}
		return IntegerValue(a / b), nil
	case "%":
		if b == 0 {
			return Null, nil
		}
		return IntegerValue(a % b), nil
	case "^":
		return FloatValue(math.Pow(float64(a), float64(b))), nil
	}
	return Null, nil
}

func evalFloatArith(op string, a, b float64) (Value, error) {
	switch op {
	case "+":
		return FloatValue(a + b), nil
	case "-":
		return FloatValue(a - b), nil
	case "*":
		return FloatValue(a * b), nil
	case "/":
		// Float division by zero → Inf/NaN (IEEE 754), not error.
		return FloatValue(a / b), nil
	case "%":
		return FloatValue(math.Mod(a, b)), nil
	case "^":
		return FloatValue(math.Pow(a, b)), nil
	}
	return Null, nil
}

// evalStringOp handles CONTAINS, STARTS WITH, ENDS WITH, =~.
func evalStringOp(op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	ls, lok := left.(StringValue)
	rs, rok := right.(StringValue)
	if !lok || !rok {
		return Null, nil
	}
	s, pattern := string(ls), string(rs)
	switch op {
	case "CONTAINS":
		return BoolValue(strings.Contains(s, pattern)), nil
	case "STARTS WITH":
		return BoolValue(strings.HasPrefix(s, pattern)), nil
	case "ENDS WITH":
		return BoolValue(strings.HasSuffix(s, pattern)), nil
	case "=~":
		matched, err := regexp.MatchString(pattern, s)
		if err != nil {
			return Null, nil //nolint:nilerr // invalid pattern → NULL per openCypher
		}
		return BoolValue(matched), nil
	}
	return Null, nil
}

// evalIn evaluates value IN list.
func evalIn(left, right Value) (Value, error) {
	if IsNull(left) {
		return Null, nil
	}
	if IsNull(right) {
		return Null, nil
	}
	list, ok := right.(ListValue)
	if !ok {
		return Null, nil
	}
	// Scan the list. Track whether we encountered any NULL to decide final result.
	sawNull := false
	for _, elem := range list {
		eq := left.Equal(elem)
		if IsTruthy(eq) {
			return BoolValue(true), nil
		}
		if IsNull(eq) {
			sawNull = true
		}
	}
	if sawNull {
		return Null, nil
	}
	return BoolValue(false), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Unary operator
// ─────────────────────────────────────────────────────────────────────────────

func evalUnaryOp(n *ast.UnaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	switch n.Operator {
	case "IS NULL":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(IsNull(operand)), nil

	case "IS NOT NULL":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(!IsNull(operand)), nil

	case "NOT":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		if IsNull(operand) {
			return Null, nil
		}
		b, ok := operand.(BoolValue)
		if !ok {
			return Null, nil
		}
		return BoolValue(!bool(b)), nil

	case "-":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		if IsNull(operand) {
			return Null, nil
		}
		switch v := operand.(type) {
		case IntegerValue:
			return IntegerValue(-int64(v)), nil
		case FloatValue:
			return FloatValue(-float64(v)), nil
		}
		return Null, nil

	case "+":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return operand, nil

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported unary operator %q", n.Operator)}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CASE expression
// ─────────────────────────────────────────────────────────────────────────────

func evalCase(n *ast.CaseExpression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	// Value-form CASE: CASE subject WHEN v1 THEN r1 … ELSE rn END
	// Generic CASE: CASE WHEN pred1 THEN r1 … ELSE rn END
	var subject Value
	if n.Subject != nil {
		var err error
		subject, err = evalExpr(n.Subject, row, params, reg)
		if err != nil {
			return nil, err
		}
	}

	for _, alt := range n.Alternatives {
		cond, err := evalExpr(alt.Condition, row, params, reg)
		if err != nil {
			return nil, err
		}
		matched := false
		if n.Subject != nil {
			// Value-form: compare subject = condition.
			eq := subject.Equal(cond)
			matched = IsTruthy(eq)
		} else {
			// Generic-form: condition must be truthy.
			matched = IsTruthy(cond)
		}
		if matched {
			return evalExpr(alt.Consequent, row, params, reg)
		}
	}

	// No arm matched: evaluate ELSE or return NULL.
	if n.ElseExpr != nil {
		return evalExpr(n.ElseExpr, row, params, reg)
	}
	return Null, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Function invocation
// ─────────────────────────────────────────────────────────────────────────────

func evalFunction(n *ast.FunctionInvocation, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	if reg == nil {
		return nil, &EvalError{Msg: fmt.Sprintf("no function registry; cannot call %s()", n.Name)}
	}

	// Resolve function name. Namespaced functions join with ".".
	name := strings.ToLower(n.Name)
	if len(n.Namespace) > 0 {
		parts := make([]string, 0, len(n.Namespace)+1)
		for _, ns := range n.Namespace {
			parts = append(parts, strings.ToLower(ns))
		}
		parts = append(parts, name)
		name = strings.Join(parts, ".")
	}

	fn, ok := reg.Resolve(name)
	if !ok {
		return nil, &EvalError{Msg: fmt.Sprintf("unknown function %q", name)}
	}

	args := make([]Value, len(n.Args))
	for i, arg := range n.Args {
		v, err := evalExpr(arg, row, params, reg)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(args)
}
