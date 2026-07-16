package search

import (
	"math"
	"reflect"
)

// floatInvalid reports whether v is NaN or ±Inf for float W types.
//
// The concrete type switch handles every BUILTIN weight type reflection-free:
// float32/float64 are range-checked and the integer kinds short-circuit to
// false. A DEFINED weight type (e.g. `type Cost float64`, permitted by the
// ~float32|~float64 arms of the Weight constraint) does not match a concrete
// case, so its underlying kind is resolved via reflection — without this a
// defined float type slips past the NaN/±Inf gate and yields a silently wrong
// result. Only defined types pay the reflection cost; the hot path used by
// the engine (concrete float64) stays reflection-free.
func floatInvalid[W Weight](v W) bool {
	switch f := any(v).(type) {
	case float64:
		return math.IsNaN(f) || math.IsInf(f, 0)
	case float32:
		g := float64(f)
		return math.IsNaN(g) || math.IsInf(g, 0)
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		return false
	default:
		switch reflect.TypeOf(v).Kind() {
		case reflect.Float32, reflect.Float64:
			g := reflect.ValueOf(v).Float()
			return math.IsNaN(g) || math.IsInf(g, 0)
		default:
			return false
		}
	}
}

// anyFloatInvalid reports whether weights contains a NaN or ±Inf value, but
// only when W is a floating-point type. The float-ness of W is decided ONCE
// on the zero value (O(1)); an integer W (builtin or defined) returns false
// without scanning, so integer weight types never pay a per-element cost.
//
// The function is used by Bellman-Ford (and other algorithms whose inner
// relaxation breaks silently on NaN/Inf) to fail fast at the public-API
// boundary with [ErrInvalidInput]. Like [floatInvalid], a DEFINED float weight
// type is handled via reflection so it is not silently exempted from the gate.
func anyFloatInvalid[W Weight](weights []W) bool {
	if len(weights) == 0 {
		return false
	}
	var zero W
	switch any(zero).(type) {
	case float64:
		for _, w := range weights {
			f := any(w).(float64) //nolint:errcheck // type-asserted by the outer switch
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return true
			}
		}
		return false
	case float32:
		for _, w := range weights {
			g := float64(any(w).(float32)) //nolint:errcheck // type-asserted by the outer switch
			if math.IsNaN(g) || math.IsInf(g, 0) {
				return true
			}
		}
		return false
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		return false
	default:
		// Defined weight type: resolve the underlying kind once.
		switch reflect.TypeOf(zero).Kind() {
		case reflect.Float32, reflect.Float64:
			for _, w := range weights {
				g := reflect.ValueOf(w).Float()
				if math.IsNaN(g) || math.IsInf(g, 0) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
}
