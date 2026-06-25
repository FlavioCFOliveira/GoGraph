package cypher_test

// conformance_round2_test.go — end-to-end regression gates for the 2026-06-25
// round-2 reliability audit conformance fixes, driven through Engine.Run:
//   #1764 toString() preserves FLOAT-ness (.0)
//   #1766 integer / and % by zero raise (float by-zero stays IEEE-754)
//   #1767 invalid calendar date components are rejected, not normalized
//   #1768 substring/left/right negative args raise ArgumentError

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func r2Engine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// r2Scalar runs `RETURN <expr> AS x` and returns the single x value.
func r2Scalar(t *testing.T, eng *cypher.Engine, expression string) expr.Value {
	t.Helper()
	res, err := eng.Run(context.Background(), "RETURN "+expression+" AS x", nil)
	if err != nil {
		t.Fatalf("RETURN %s: Run error %v", expression, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("RETURN %s: no row", expression)
	}
	v, _ := res.Record()["x"].(expr.Value)
	if err := res.Err(); err != nil {
		t.Fatalf("RETURN %s: iter error %v", expression, err)
	}
	return v
}

// r2ExpectErr runs `RETURN <expr> AS x` and asserts it fails (at Run or during
// iteration) with an error containing want.
func r2ExpectErr(t *testing.T, eng *cypher.Engine, expression, want string) {
	t.Helper()
	res, err := eng.Run(context.Background(), "RETURN "+expression+" AS x", nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		_ = res.Close()
	}
	if err == nil {
		t.Fatalf("RETURN %s: expected an error containing %q, got none", expression, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("RETURN %s: error = %q, want it to contain %q", expression, err.Error(), want)
	}
}

func TestR2_ToString_PreservesFloatDotZero(t *testing.T) { // #1764
	t.Parallel()
	eng := r2Engine(t)
	for expression, want := range map[string]string{
		"toString(1.0)":   "1.0",
		"toString(100.0)": "100.0",
		"toString(0.0)":   "0.0",
		"toString(1.5)":   "1.5", // non-integer float unchanged
	} {
		got, ok := r2Scalar(t, eng, expression).(expr.StringValue)
		if !ok || string(got) != want {
			t.Errorf("%s = %v, want %q", expression, got, want)
		}
	}
}

func TestR2_IntDivModByZero_Raise_FloatStaysIEEE(t *testing.T) { // #1766
	t.Parallel()
	eng := r2Engine(t)
	r2ExpectErr(t, eng, "5 / 0", "ArithmeticError")
	r2ExpectErr(t, eng, "5 % 0", "ArithmeticError")
	// Float by-zero is IEEE-754 and must still succeed (+Inf / NaN).
	if v, ok := r2Scalar(t, eng, "5.0 / 0.0").(expr.FloatValue); !ok || !math.IsInf(float64(v), 1) {
		t.Errorf("5.0/0.0 = %v, want +Inf", r2Scalar(t, eng, "5.0 / 0.0"))
	}
	if v, ok := r2Scalar(t, eng, "5.0 % 0.0").(expr.FloatValue); !ok || !math.IsNaN(float64(v)) {
		t.Errorf("5.0 %% 0.0 = %v, want NaN", r2Scalar(t, eng, "5.0 % 0.0"))
	}
}

func TestR2_InvalidDateComponents_Rejected(t *testing.T) { // #1767
	t.Parallel()
	eng := r2Engine(t)
	// Out-of-range components must NOT silently normalize into a valid (wrong)
	// date. The engine surfaces any invalid date string as null (the same
	// contract as date('garbage')), so the fix makes these null instead of
	// 2021-01-01 / 2020-03-01 — the silent wrong-result is gone.
	for _, bad := range []string{"date('2020-13-01')", "date('2020-02-30')"} {
		if v := r2Scalar(t, eng, bad); !expr.IsNull(v) {
			t.Errorf("%s = %v, want null (must not normalize to a valid wrong date)", bad, v)
		}
	}
	// Legitimate trailing-component truncation still works.
	if d, ok := r2Scalar(t, eng, "date('2013-06')").(expr.DateValue); !ok || d.Year != 2013 || d.Month != 6 || d.Day != 1 {
		t.Errorf("date('2013-06') = %v, want 2013-06-01", r2Scalar(t, eng, "date('2013-06')"))
	}
}

func TestR2_SubstringFamily_NegativeArgs_Raise(t *testing.T) { // #1768
	t.Parallel()
	eng := r2Engine(t)
	r2ExpectErr(t, eng, "substring('hello', -1)", "ArgumentError")
	r2ExpectErr(t, eng, "substring('hello', 0, -1)", "ArgumentError")
	r2ExpectErr(t, eng, "left('hello', -1)", "ArgumentError")
	r2ExpectErr(t, eng, "right('hello', -1)", "ArgumentError")
	// start beyond the end is a value query (empty), not an error.
	if s, ok := r2Scalar(t, eng, "substring('hello', 10)").(expr.StringValue); !ok || string(s) != "" {
		t.Errorf("substring('hello',10) = %v, want \"\"", r2Scalar(t, eng, "substring('hello', 10)"))
	}
}
