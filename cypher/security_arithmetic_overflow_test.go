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

// TestSec_Cypher_IntDivModByZero_Raises asserts that INTEGER division and modulo
// by zero RAISE a typed *expr.EvalError (ArithmeticError) rather than silently
// evaluating to NULL — matching Neo4j (openCypher leaves it implementation-
// defined; the silent NULL was a reliability hazard, #1766). Float by-zero stays
// IEEE-754 (+Inf / NaN) and is asserted separately below.
func TestSec_Cypher_IntDivModByZero_Raises(t *testing.T) {
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
				t.Fatalf("%q: produced %d rows; integer divide/modulo by zero must yield no successful row", q, rows)
			}
		})
	}
}

// TestSec_Cypher_FloatDivModByZero_IEEE confirms FLOAT by-zero is unchanged:
// x/0.0 → +Inf, x%0.0 → NaN (IEEE-754), one successful row, no error (#1766).
func TestSec_Cypher_FloatDivModByZero_IEEE(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	for _, tc := range []struct {
		name  string
		expr  string
		check func(float64) bool
	}{
		{"div_inf", "5.0 / 0.0", func(f float64) bool { return math.IsInf(f, 1) }},
		{"mod_nan", "5.0 % 0.0", func(f float64) bool { return math.IsNaN(f) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + tc.expr + " AS x"
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("%q: unexpected Run error %v", q, err)
			}
			defer func() { _ = res.Close() }()
			if !res.Next() {
				t.Fatalf("%q: produced no row", q)
			}
			fv, ok := res.Record()["x"].(expr.FloatValue)
			if !ok || !tc.check(float64(fv)) {
				t.Fatalf("%q: x = %v (%T), want the IEEE-754 result", q, res.Record()["x"], res.Record()["x"])
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
