package funcs

// math_funcs.go — extended math built-ins for the Cypher function registry (task-266).
//
// Adds: exp, log, log10, sin, cos, tan, asin, acos, atan, atan2,
//       degrees, radians, pi, e, rand.
//
// All single-argument trig/exp/log functions accept Integer or Float,
// promote Integer to Float, and return FloatValue. NULL propagates.

import (
	"math"
	"math/rand/v2"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// registerMathFuncs registers extended math built-ins into r.
func registerMathFuncs(r *Registry) {
	r.Register("exp", fnExp)
	r.Register("log", fnLog)
	r.Register("log10", fnLog10)
	r.Register("sin", fnSin)
	r.Register("cos", fnCos)
	r.Register("tan", fnTan)
	r.Register("asin", fnAsin)
	r.Register("acos", fnAcos)
	r.Register("atan", fnAtan)
	r.Register("atan2", fnAtan2)
	r.Register("degrees", fnDegrees)
	r.Register("radians", fnRadians)
	r.Register("pi", fnPi)
	r.Register("e", fnE)
	r.Register("rand", fnRand)
}

// unaryFloatFn builds a BuiltinFn that applies f to a single numeric argument.
func unaryFloatFn(name string, f func(float64) float64) expr.BuiltinFn {
	return func(args []expr.Value) (expr.Value, error) {
		if err := requireArity(name, args, 1); err != nil {
			return nil, err
		}
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		x, ok := toFloat64(args[0])
		if !ok {
			return nil, &TypeError{Function: name, ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
		}
		return expr.FloatValue(f(x)), nil
	}
}

var (
	fnExp     = unaryFloatFn("exp", math.Exp)
	fnLog     = unaryFloatFn("log", math.Log)
	fnLog10   = unaryFloatFn("log10", math.Log10)
	fnSin     = unaryFloatFn("sin", math.Sin)
	fnCos     = unaryFloatFn("cos", math.Cos)
	fnTan     = unaryFloatFn("tan", math.Tan)
	fnAsin    = unaryFloatFn("asin", math.Asin)
	fnAcos    = unaryFloatFn("acos", math.Acos)
	fnAtan    = unaryFloatFn("atan", math.Atan)
	fnDegrees = unaryFloatFn("degrees", func(r float64) float64 { return r * 180 / math.Pi })
	fnRadians = unaryFloatFn("radians", func(d float64) float64 { return d * math.Pi / 180 })
)

func fnAtan2(args []expr.Value) (expr.Value, error) {
	if err := requireArity("atan2", args, 2); err != nil {
		return nil, err
	}
	for i, a := range args {
		if expr.IsNull(a) {
			return expr.Null, nil
		}
		if _, ok := toFloat64(a); !ok {
			return nil, &TypeError{Function: "atan2", ArgIndex: i, Got: a.Kind(), Want: "Float or Integer"}
		}
	}
	y, _ := toFloat64(args[0]) //nolint:forcetypeassert // type-checked above
	x, _ := toFloat64(args[1]) //nolint:forcetypeassert // type-checked above
	return expr.FloatValue(math.Atan2(y, x)), nil
}

// fnPi returns the mathematical constant π. Accepts zero arguments.
func fnPi(args []expr.Value) (expr.Value, error) {
	if err := requireArity("pi", args, 0); err != nil {
		return nil, err
	}
	return expr.FloatValue(math.Pi), nil
}

// fnE returns Euler's number e. Accepts zero arguments.
func fnE(args []expr.Value) (expr.Value, error) {
	if err := requireArity("e", args, 0); err != nil {
		return nil, err
	}
	return expr.FloatValue(math.E), nil
}

// fnRand returns a random float in [0, 1). Accepts zero arguments.
func fnRand(args []expr.Value) (expr.Value, error) {
	if err := requireArity("rand", args, 0); err != nil {
		return nil, err
	}
	return expr.FloatValue(rand.Float64()), nil //nolint:gosec // non-cryptographic random is correct for Cypher rand()
}
