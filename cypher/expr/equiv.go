package expr

// equiv.go — value equivalence for DISTINCT and grouping (openCypher CIP2016-06-14).
//
// openCypher distinguishes two notions of equality:
//
//   - Equality (=): IEEE-754 semantics for floats (NaN ≠ NaN), three-valued
//     logic for NULL (NULL = x → NULL). Used in WHERE predicates and
//     comparison expressions. Implemented by [Value.Equal].
//
//   - Equivalence (≡): used by DISTINCT deduplication and grouping.
//     NaN ≡ NaN, null ≡ null (even inside lists and maps), two-valued boolean
//     (always true or false, never null). Implemented by [Equivalent].
//
// The split is required by openCypher CIP2016-06-14 §§393–394, 448–449.
// [Value.Equal] must NOT be changed; callers that need grouping/dedup
// semantics must call [Equivalent] instead of IsTruthy(a.Equal(b)).

import "math"

// Equivalent reports whether a and b are equivalent for DISTINCT and grouping
// purposes (openCypher CIP2016-06-14).
//
// Key differences from IsTruthy(a.Equal(b)):
//   - null ≡ null → true (also inside lists and maps)
//   - NaN ≡ NaN → true
//   - null ≢ NaN → false
//   - [1, null] ≡ [1, null] → true
func Equivalent(a, b Value) bool {
	aN, bN := IsNull(a), IsNull(b)
	if aN && bN {
		return true
	}
	if aN || bN {
		return false
	}

	// Both non-null. Dispatch on concrete types that need special treatment.
	switch av := a.(type) {
	case FloatValue:
		bv, ok := b.(FloatValue)
		if !ok {
			// cross-type float/int: delegate to Equal (no NaN issue for integers)
			return IsTruthy(a.Equal(b))
		}
		aNaN := math.IsNaN(float64(av))
		bNaN := math.IsNaN(float64(bv))
		if aNaN || bNaN {
			return aNaN && bNaN // NaN ≡ NaN; NaN ≢ finite
		}
		return float64(av) == float64(bv)

	case ListValue:
		bv, ok := b.(ListValue)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !Equivalent(av[i], bv[i]) {
				return false
			}
		}
		return true

	case MapValue:
		bv, ok := b.(MapValue)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, aval := range av {
			bval, exists := bv[k]
			if !exists {
				return false
			}
			if !Equivalent(aval, bval) {
				return false
			}
		}
		return true
	}

	// All other types (Integer, String, Bool, Node, Relationship, Path,
	// temporal types): use Equal — none of them have NaN or null-propagation
	// issues so IsTruthy(Equal) gives the right answer.
	return IsTruthy(a.Equal(b))
}

// EquivalentHash returns a hash for v that is consistent with [Equivalent]:
// two values that are Equivalent always produce the same EquivalentHash.
//
// This differs from v.Hash() for FloatValue in two cases:
//   - All NaN bit-patterns map to one canonical hash (NaN ≡ NaN).
//   - -0.0 maps to the same hash as 0.0 (−0.0 == 0.0 in IEEE 754).
func EquivalentHash(v Value) uint64 {
	if fv, ok := v.(FloatValue); ok {
		f := float64(fv)
		if math.IsNaN(f) {
			// Canonical NaN hash: fixed non-zero constant so all NaN
			// bit-patterns land in the same bucket.
			const nanHash uint64 = 0x7FF8000000000001 // canonical qNaN bits
			return nanHash ^ (nanHash >> 32)
		}
		// Canonicalise -0.0 → +0.0 so both map to the same hash.
		// (IEEE 754: -0.0 == +0.0, so they must be equivalent.)
		if f == 0 {
			f = 0.0 // force positive zero bit pattern
		}
		bits := math.Float64bits(f)
		return bits ^ (bits >> 32)
	}
	if lv, ok := v.(ListValue); ok {
		const (
			offset uint64 = 14695981039346656037
			prime  uint64 = 1099511628211
		)
		h := offset
		for _, elem := range lv {
			h = h*prime ^ EquivalentHash(elem)
		}
		return h
	}
	if mv, ok := v.(MapValue); ok {
		var h uint64
		for k, val := range mv {
			kh := StringValue(k).Hash()
			h ^= kh*1099511628211 ^ EquivalentHash(val)
		}
		return h
	}
	return v.Hash()
}

// HashRowEquivalent returns the equivalence-consistent hash for a row (slice of
// Values). It uses [EquivalentHash] for each element so that rows containing
// NaN or null-bearing lists/maps hash consistently with [Equivalent].
func HashRowEquivalent(row []Value) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	for _, v := range row {
		h = h*prime ^ EquivalentHash(v)
	}
	return h
}
