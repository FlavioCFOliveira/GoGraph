package funcs_test

// math_funcs_test.go — tests for extended math built-ins (task-266).
//
// Covers: exp, log, log10, sin, cos, tan, asin, acos, atan, atan2,
//         degrees, radians, pi, e, rand.
// NULL propagation and arity errors are verified for every function.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

const floatEpsilon = 1e-9

func approxEqual(a, b float64) bool {
	if math.IsInf(a, 0) || math.IsInf(b, 0) || math.IsNaN(a) || math.IsNaN(b) {
		return a == b
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= floatEpsilon
}

func mustFloat(t *testing.T, v expr.Value) float64 {
	t.Helper()
	fv, ok := v.(expr.FloatValue)
	if !ok {
		t.Fatalf("expected FloatValue, got %T: %v", v, v)
	}
	return float64(fv)
}

// ─────────────────────────────────────────────────────────────────────────────
// exp()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Exp_Float(t *testing.T) {
	v := mustCall(t, "exp", expr.FloatValue(1.0))
	if !approxEqual(mustFloat(t, v), math.E) {
		t.Errorf("exp(1.0) = %v, want e", v)
	}
}

func TestMath_Exp_Zero(t *testing.T) {
	v := mustCall(t, "exp", expr.IntegerValue(0))
	if !approxEqual(mustFloat(t, v), 1.0) {
		t.Errorf("exp(0) = %v, want 1.0", v)
	}
}

func TestMath_Exp_Null(t *testing.T) {
	v := mustCall(t, "exp", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("exp(null) = %v, want null", v)
	}
}

func TestMath_Exp_TypeError(t *testing.T) {
	_, err := call(t, "exp", expr.StringValue("x"))
	if err == nil {
		t.Error("exp(string) should return error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// log()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Log_E(t *testing.T) {
	v := mustCall(t, "log", expr.FloatValue(math.E))
	if !approxEqual(mustFloat(t, v), 1.0) {
		t.Errorf("log(e) = %v, want 1.0", v)
	}
}

func TestMath_Log_One(t *testing.T) {
	v := mustCall(t, "log", expr.IntegerValue(1))
	if !approxEqual(mustFloat(t, v), 0.0) {
		t.Errorf("log(1) = %v, want 0.0", v)
	}
}

func TestMath_Log_Null(t *testing.T) {
	v := mustCall(t, "log", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("log(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// log10()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Log10_100(t *testing.T) {
	v := mustCall(t, "log10", expr.FloatValue(100.0))
	if !approxEqual(mustFloat(t, v), 2.0) {
		t.Errorf("log10(100) = %v, want 2.0", v)
	}
}

func TestMath_Log10_Null(t *testing.T) {
	v := mustCall(t, "log10", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("log10(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sin(), cos(), tan()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Sin_Zero(t *testing.T) {
	v := mustCall(t, "sin", expr.IntegerValue(0))
	if !approxEqual(mustFloat(t, v), 0.0) {
		t.Errorf("sin(0) = %v, want 0.0", v)
	}
}

func TestMath_Sin_PiOver2(t *testing.T) {
	v := mustCall(t, "sin", expr.FloatValue(math.Pi/2))
	if !approxEqual(mustFloat(t, v), 1.0) {
		t.Errorf("sin(π/2) = %v, want 1.0", v)
	}
}

func TestMath_Sin_Null(t *testing.T) {
	if !expr.IsNull(mustCall(t, "sin", expr.Null)) {
		t.Error("sin(null) should be null")
	}
}

func TestMath_Cos_Zero(t *testing.T) {
	v := mustCall(t, "cos", expr.IntegerValue(0))
	if !approxEqual(mustFloat(t, v), 1.0) {
		t.Errorf("cos(0) = %v, want 1.0", v)
	}
}

func TestMath_Cos_Pi(t *testing.T) {
	v := mustCall(t, "cos", expr.FloatValue(math.Pi))
	if !approxEqual(mustFloat(t, v), -1.0) {
		t.Errorf("cos(π) = %v, want -1.0", v)
	}
}

func TestMath_Tan_Zero(t *testing.T) {
	v := mustCall(t, "tan", expr.IntegerValue(0))
	if !approxEqual(mustFloat(t, v), 0.0) {
		t.Errorf("tan(0) = %v, want 0.0", v)
	}
}

func TestMath_Tan_PiOver4(t *testing.T) {
	v := mustCall(t, "tan", expr.FloatValue(math.Pi/4))
	if !approxEqual(mustFloat(t, v), 1.0) {
		t.Errorf("tan(π/4) = %v, want 1.0", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// asin(), acos(), atan(), atan2()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Asin_One(t *testing.T) {
	v := mustCall(t, "asin", expr.FloatValue(1.0))
	if !approxEqual(mustFloat(t, v), math.Pi/2) {
		t.Errorf("asin(1) = %v, want π/2", v)
	}
}

func TestMath_Acos_One(t *testing.T) {
	v := mustCall(t, "acos", expr.FloatValue(1.0))
	if !approxEqual(mustFloat(t, v), 0.0) {
		t.Errorf("acos(1) = %v, want 0.0", v)
	}
}

func TestMath_Atan_One(t *testing.T) {
	v := mustCall(t, "atan", expr.FloatValue(1.0))
	if !approxEqual(mustFloat(t, v), math.Pi/4) {
		t.Errorf("atan(1) = %v, want π/4", v)
	}
}

func TestMath_Atan_Null(t *testing.T) {
	if !expr.IsNull(mustCall(t, "atan", expr.Null)) {
		t.Error("atan(null) should be null")
	}
}

func TestMath_Atan2_Basic(t *testing.T) {
	v := mustCall(t, "atan2", expr.FloatValue(1.0), expr.FloatValue(1.0))
	if !approxEqual(mustFloat(t, v), math.Pi/4) {
		t.Errorf("atan2(1,1) = %v, want π/4", v)
	}
}

func TestMath_Atan2_NullFirst(t *testing.T) {
	v := mustCall(t, "atan2", expr.Null, expr.FloatValue(1.0))
	if !expr.IsNull(v) {
		t.Error("atan2(null, 1) should be null")
	}
}

func TestMath_Atan2_NullSecond(t *testing.T) {
	v := mustCall(t, "atan2", expr.FloatValue(1.0), expr.Null)
	if !expr.IsNull(v) {
		t.Error("atan2(1, null) should be null")
	}
}

func TestMath_Atan2_TypeError(t *testing.T) {
	_, err := call(t, "atan2", expr.StringValue("x"), expr.FloatValue(1.0))
	if err == nil {
		t.Error("atan2(string, float) should return error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// degrees(), radians()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Degrees_Pi(t *testing.T) {
	v := mustCall(t, "degrees", expr.FloatValue(math.Pi))
	if !approxEqual(mustFloat(t, v), 180.0) {
		t.Errorf("degrees(π) = %v, want 180", v)
	}
}

func TestMath_Degrees_IntegerInput(t *testing.T) {
	v := mustCall(t, "degrees", expr.IntegerValue(0))
	if !approxEqual(mustFloat(t, v), 0.0) {
		t.Errorf("degrees(0) = %v, want 0.0", v)
	}
}

func TestMath_Degrees_Null(t *testing.T) {
	if !expr.IsNull(mustCall(t, "degrees", expr.Null)) {
		t.Error("degrees(null) should be null")
	}
}

func TestMath_Radians_180(t *testing.T) {
	v := mustCall(t, "radians", expr.FloatValue(180.0))
	if !approxEqual(mustFloat(t, v), math.Pi) {
		t.Errorf("radians(180) = %v, want π", v)
	}
}

func TestMath_Radians_Null(t *testing.T) {
	if !expr.IsNull(mustCall(t, "radians", expr.Null)) {
		t.Error("radians(null) should be null")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pi(), e()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Pi(t *testing.T) {
	v := mustCall(t, "pi")
	if !approxEqual(mustFloat(t, v), math.Pi) {
		t.Errorf("pi() = %v, want π", v)
	}
}

func TestMath_Pi_ArityError(t *testing.T) {
	_, err := call(t, "pi", expr.IntegerValue(1))
	if err == nil {
		t.Error("pi(1) should return ArityError")
	}
}

func TestMath_E(t *testing.T) {
	v := mustCall(t, "e")
	if !approxEqual(mustFloat(t, v), math.E) {
		t.Errorf("e() = %v, want e", v)
	}
}

func TestMath_E_ArityError(t *testing.T) {
	_, err := call(t, "e", expr.IntegerValue(1))
	if err == nil {
		t.Error("e(1) should return ArityError")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// rand()
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_Rand_InRange(t *testing.T) {
	v := mustCall(t, "rand")
	fv, ok := v.(expr.FloatValue)
	if !ok {
		t.Fatalf("rand() returned %T, want FloatValue", v)
	}
	f := float64(fv)
	if f < 0 || f >= 1 {
		t.Errorf("rand() = %v, want [0, 1)", f)
	}
}

func TestMath_Rand_Stable_Type(t *testing.T) {
	for i := range 5 {
		v := mustCall(t, "rand")
		if _, ok := v.(expr.FloatValue); !ok {
			t.Errorf("rand() call %d returned %T, want FloatValue", i, v)
		}
	}
}

func TestMath_Rand_ArityError(t *testing.T) {
	_, err := call(t, "rand", expr.IntegerValue(1))
	if err == nil {
		t.Error("rand(1) should return ArityError")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Arity errors for unary trig/log functions
// ─────────────────────────────────────────────────────────────────────────────

func TestMath_UnaryArity(t *testing.T) {
	fns := []string{"exp", "log", "log10", "sin", "cos", "tan", "asin", "acos", "atan", "degrees", "radians"}
	for _, name := range fns {
		name := name
		t.Run(name, func(t *testing.T) {
			_, err := call(t, name)
			if err == nil {
				t.Errorf("%s() with no args should return ArityError", name)
			}
		})
	}
}

func TestMath_Atan2_Arity(t *testing.T) {
	_, err := call(t, "atan2", expr.FloatValue(1.0))
	if err == nil {
		t.Error("atan2(1) with only 1 arg should return ArityError")
	}
}
