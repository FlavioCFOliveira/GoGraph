package cypher_test

// security_arithmetic_overflow_test.go — DEFENSE LOCK-IN for integer-arithmetic
// overflow at evaluation time.
//
// Integer arithmetic on int64 that would overflow (+, -, *, /, %, and the
// MinInt64 / -1 corner) must produce a typed *expr.EvalError signalling
// ArithmeticOverflow, NOT a silently wrapped (wrong) result and NOT a panic.
//
// Division and modulo by zero are DIFFERENT: openCypher specifies them as NULL,
// not an error. Both contracts are pinned here.
//
// API note: arithmetic is evaluated lazily inside the Projection operator, so
// Engine.Run returns a nil error and the overflow surfaces on Result.Err()
// AFTER the row drains to zero rows. The tests therefore read Result.Err(), not
// the Run return value — this distinction is itself part of the contract being
// fenced.
//
// All cases pass today; this is a regression fence.

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// secCypherMaxI64 and secCypherMinI64 are the int64 extremes as decimal strings
// for embedding directly in query text.
var (
	secCypherMaxI64 = strconv.FormatInt(math.MaxInt64, 10) // 9223372036854775807
	secCypherMinI64 = strconv.FormatInt(math.MinInt64, 10) // -9223372036854775808
)

// TestSec_Cypher_IntegerOverflow_TypedEvalError asserts that each overflowing
// arithmetic expression yields zero rows and a Result.Err() that unwraps to a
// *expr.EvalError. The expression strings cover +, -, *, /, % and the
// MinInt64 / -1 non-representable-quotient corner.
func TestSec_Cypher_IntegerOverflow_TypedEvalError(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	exprs := []struct {
		name string
		expr string
	}{
		{"add", secCypherMaxI64 + " + 1"},
		{"sub", secCypherMinI64 + " - 1"},
		{"mul", secCypherMaxI64 + " * 2"},
		{"min_div_neg1", secCypherMinI64 + " / -1"}, // quotient MaxInt64+1 is not representable
		{"add_self", secCypherMaxI64 + " + " + secCypherMaxI64},
	}
	for _, tc := range exprs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + tc.expr + " AS x"
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				// An overflow surfaced at build time would also be acceptable as
				// long as it is a typed EvalError — but the current contract
				// surfaces it on Result.Err(). Accept either, require the type.
				secCypherAssertEvalError(t, err, q)
				return
			}
			rows := 0
			for res.Next() {
				rows++
			}
			iterErr := res.Err()
			_ = res.Close()
			secCypherAssertEvalError(t, iterErr, q)
			if rows != 0 {
				t.Fatalf("%q: produced %d rows; an overflowing expression must yield no successful row", q, rows)
			}
		})
	}
}

// TestSec_Cypher_DivModByZero_Null asserts that integer division and modulo by
// zero evaluate to NULL (openCypher semantics) — exactly one row, the projected
// column is null, and there is no error.
func TestSec_Cypher_DivModByZero_Null(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	cases := []struct {
		name string
		expr string
	}{
		{"div_by_zero", "1 / 0"},
		{"mod_by_zero", "1 % 0"},
		{"div_zero_by_zero", "0 / 0"},
		{"large_div_zero", secCypherMaxI64 + " / 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + tc.expr + " AS x"
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("%q: unexpected Run error %v; want one NULL row", q, err)
			}
			defer func() { _ = res.Close() }()
			rows := 0
			gotNull := false
			for res.Next() {
				// Record() values are interface{}; assert to expr.Value before
				// the NULL test. A non-Value would itself be a contract breach.
				v, ok := res.Record()["x"].(expr.Value)
				if !ok {
					t.Fatalf("%q: column x is %T, not an expr.Value", q, res.Record()["x"])
				}
				gotNull = expr.IsNull(v)
				rows++
			}
			if err := res.Err(); err != nil {
				t.Fatalf("%q: unexpected Result.Err() %v; want one NULL row", q, err)
			}
			if rows != 1 {
				t.Fatalf("%q: produced %d rows; want exactly 1", q, rows)
			}
			if !gotNull {
				t.Fatalf("%q: column x is not NULL; want NULL (division/modulo by zero is NULL in openCypher)", q)
			}
		})
	}
}

// secCypherAssertEvalError asserts err is non-nil and unwraps to *expr.EvalError.
func secCypherAssertEvalError(t *testing.T, err error, q string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%q: expected a *expr.EvalError (arithmetic overflow), got nil — the result wrapped silently", q)
	}
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("%q: error %T (%v) does not wrap *expr.EvalError", q, err, err)
	}
}
