package expr_test

// eval_overflow_test.go — gate tests for integer arithmetic overflow detection.
//
// Verifies that evalIntArith, unary minus, and the funcs-layer abs/toInteger
// all return ArithmeticOverflow EvalErrors for out-of-range inputs while
// leaving normal in-range operations intact.

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// evalBinaryInt evaluates a binary integer operation via the full Eval path so
// we go through evalIntArith without exposing it directly.
func evalBinaryInt(t *testing.T, op string, a, b int64) (expr.Value, error) {
	t.Helper()
	node := &ast.BinaryOp{
		Operator: op,
		Left:     &ast.IntLiteral{Value: a},
		Right:    &ast.IntLiteral{Value: b},
	}
	return expr.Eval(node, nil, nil, nil)
}

// evalUnaryMinus evaluates unary negation of an integer literal.
func evalUnaryMinus(t *testing.T, v int64) (expr.Value, error) {
	t.Helper()
	node := &ast.UnaryOp{
		Operator: "-",
		Operand:  &ast.IntLiteral{Value: v},
	}
	return expr.Eval(node, nil, nil, nil)
}

func isArithmeticOverflow(err error) bool {
	var e *expr.EvalError
	if !errors.As(err, &e) {
		return false
	}
	const prefix = "ArithmeticOverflow: "
	return len(e.Msg) >= len(prefix) && e.Msg[:len(prefix)] == prefix
}

// ─────────────────────────────────────────────────────────────────────────────
// Binary arithmetic overflow
// ─────────────────────────────────────────────────────────────────────────────

func TestIntArith_Overflow(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		op      string
		a, b    int64
		wantErr bool // true → expect ArithmeticOverflow error
		wantVal int64
	}

	cases := []tc{
		// Overflow cases
		{name: "MaxInt64+1", op: "+", a: math.MaxInt64, b: 1, wantErr: true},
		{name: "MinInt64-1", op: "-", a: math.MinInt64, b: 1, wantErr: true},
		{name: "MaxInt64*2", op: "*", a: math.MaxInt64, b: 2, wantErr: true},
		{name: "MinInt64*(-1)", op: "*", a: math.MinInt64, b: -1, wantErr: true},
		{name: "MaxInt64+MaxInt64", op: "+", a: math.MaxInt64, b: math.MaxInt64, wantErr: true},
		{name: "MinInt64+MinInt64", op: "+", a: math.MinInt64, b: math.MinInt64, wantErr: true},
		{name: "MinInt64-MaxInt64", op: "-", a: math.MinInt64, b: math.MaxInt64, wantErr: true},
		{name: "MaxInt64-MinInt64", op: "-", a: math.MaxInt64, b: math.MinInt64, wantErr: true},
		{name: "MaxInt64*MaxInt64", op: "*", a: math.MaxInt64, b: math.MaxInt64, wantErr: true},

		// In-range cases
		{name: "1+1", op: "+", a: 1, b: 1, wantVal: 2},
		{name: "MaxInt64+0", op: "+", a: math.MaxInt64, b: 0, wantVal: math.MaxInt64},
		{name: "MinInt64+0", op: "+", a: math.MinInt64, b: 0, wantVal: math.MinInt64},
		{name: "MaxInt64-1", op: "-", a: math.MaxInt64, b: 1, wantVal: math.MaxInt64 - 1},
		{name: "MinInt64+1", op: "+", a: math.MinInt64, b: 1, wantVal: math.MinInt64 + 1},
		{name: "100*100", op: "*", a: 100, b: 100, wantVal: 10000},
		{name: "0*MaxInt64", op: "*", a: 0, b: math.MaxInt64, wantVal: 0},
		{name: "MinInt64*1", op: "*", a: math.MinInt64, b: 1, wantVal: math.MinInt64},
		{name: "MinInt64*0", op: "*", a: math.MinInt64, b: 0, wantVal: 0},
		// Division and modulo: no overflow checks — division by zero → NULL, not error.
		{name: "10/3", op: "/", a: 10, b: 3, wantVal: 3},
		{name: "10%3", op: "%", a: 10, b: 3, wantVal: 1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			val, err := evalBinaryInt(t, tc.op, tc.a, tc.b)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected ArithmeticOverflow error, got value %v", val)
				}
				if !isArithmeticOverflow(err) {
					t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, ok := val.(expr.IntegerValue)
			if !ok {
				t.Fatalf("expected IntegerValue, got %T", val)
			}
			if int64(got) != tc.wantVal {
				t.Fatalf("expected %d, got %d", tc.wantVal, int64(got))
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unary minus overflow
// ─────────────────────────────────────────────────────────────────────────────

func TestUnaryMinus_Overflow(t *testing.T) {
	t.Parallel()

	t.Run("MinInt64 overflows", func(t *testing.T) {
		t.Parallel()
		_, err := evalUnaryMinus(t, math.MinInt64)
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for -MinInt64")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("MaxInt64 no overflow", func(t *testing.T) {
		t.Parallel()
		val, err := evalUnaryMinus(t, math.MaxInt64)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := val.(expr.IntegerValue)
		if !ok {
			t.Fatalf("expected IntegerValue, got %T", val)
		}
		if int64(got) != -math.MaxInt64 {
			t.Fatalf("expected %d, got %d", -math.MaxInt64, int64(got))
		}
	})

	t.Run("zero no overflow", func(t *testing.T) {
		t.Parallel()
		val, err := evalUnaryMinus(t, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := val.(expr.IntegerValue)
		if !ok {
			t.Fatalf("expected IntegerValue, got %T", val)
		}
		if int64(got) != 0 {
			t.Fatalf("expected 0, got %d", int64(got))
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// funcs.abs overflow
// ─────────────────────────────────────────────────────────────────────────────

func TestFnAbs_Overflow(t *testing.T) {
	t.Parallel()

	reg := funcs.DefaultRegistry

	callAbs := func(v expr.Value) (expr.Value, error) {
		fn, ok := reg.Resolve("abs")
		if !ok {
			t.Fatal("abs not in registry")
		}
		return fn([]expr.Value{v})
	}

	t.Run("abs(MinInt64) overflows", func(t *testing.T) {
		t.Parallel()
		_, err := callAbs(expr.IntegerValue(math.MinInt64))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for abs(MinInt64)")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("abs(-1) = 1", func(t *testing.T) {
		t.Parallel()
		val, err := callAbs(expr.IntegerValue(-1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := int64(val.(expr.IntegerValue)); got != 1 {
			t.Fatalf("expected 1, got %d", got)
		}
	})

	t.Run("abs(MaxInt64) = MaxInt64", func(t *testing.T) {
		t.Parallel()
		val, err := callAbs(expr.IntegerValue(math.MaxInt64))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := int64(val.(expr.IntegerValue)); got != math.MaxInt64 {
			t.Fatalf("expected %d, got %d", int64(math.MaxInt64), got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// funcs.toInteger overflow for out-of-range float
// ─────────────────────────────────────────────────────────────────────────────

func TestFnToInteger_FloatOverflow(t *testing.T) {
	t.Parallel()

	reg := funcs.DefaultRegistry

	callToInteger := func(v expr.Value) (expr.Value, error) {
		fn, ok := reg.Resolve("toInteger")
		if !ok {
			t.Fatal("toInteger not in registry")
		}
		return fn([]expr.Value{v})
	}

	t.Run("1e300 overflows", func(t *testing.T) {
		t.Parallel()
		_, err := callToInteger(expr.FloatValue(1e300))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for toInteger(1e300)")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("-1e300 overflows", func(t *testing.T) {
		t.Parallel()
		_, err := callToInteger(expr.FloatValue(-1e300))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for toInteger(-1e300)")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("+Inf overflows", func(t *testing.T) {
		t.Parallel()
		_, err := callToInteger(expr.FloatValue(math.Inf(1)))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for toInteger(+Inf)")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("NaN overflows", func(t *testing.T) {
		t.Parallel()
		_, err := callToInteger(expr.FloatValue(math.NaN()))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow error for toInteger(NaN)")
		}
		if !isArithmeticOverflow(err) {
			t.Fatalf("expected ArithmeticOverflow EvalError, got %T: %v", err, err)
		}
	})

	t.Run("2.9 → 2 (in range)", func(t *testing.T) {
		t.Parallel()
		val, err := callToInteger(expr.FloatValue(2.9))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := int64(val.(expr.IntegerValue)); got != 2 {
			t.Fatalf("expected 2, got %d", got)
		}
	})

	t.Run("-2.9 → -2 (in range)", func(t *testing.T) {
		t.Parallel()
		val, err := callToInteger(expr.FloatValue(-2.9))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := int64(val.(expr.IntegerValue)); got != -2 {
			t.Fatalf("expected -2, got %d", got)
		}
	})
}
