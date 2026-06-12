package search

import "math"

// floatInvalid reports whether v is NaN or ±Inf for float W types.
// For integer W it returns false in O(1) via an O(1) type-switch on the
// zero value.
func floatInvalid[W Weight](v W) bool {
	var zero W
	switch any(zero).(type) {
	case float32:
		f := float64(any(v).(float32)) //nolint:errcheck // type-asserted by the outer switch
		return math.IsNaN(f) || math.IsInf(f, 0)
	case float64:
		f := any(v).(float64) //nolint:errcheck // type-asserted by the outer switch
		return math.IsNaN(f) || math.IsInf(f, 0)
	}
	return false
}

// anyFloatInvalid reports whether weights contains a NaN or +/-Inf
// value, but only when W is a floating-point type. For integer W it
// returns false immediately after an O(1) type-switch on the zero
// value: integer Weight types do not pay the per-element scan cost.
//
// The function is used by Bellman-Ford (and other algorithms whose
// inner relaxation breaks silently on NaN/Inf) to fail fast at the
// public-API boundary with [ErrInvalidInput].
func anyFloatInvalid[W Weight](weights []W) bool {
	if len(weights) == 0 {
		return false
	}
	var zero W
	switch any(zero).(type) {
	case float32:
		for _, w := range weights {
			f := float64(any(w).(float32)) //nolint:errcheck // type-asserted by the outer switch
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return true
			}
		}
	case float64:
		for _, w := range weights {
			f := any(w).(float64) //nolint:errcheck // type-asserted by the outer switch
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return true
			}
		}
	}
	return false
}
