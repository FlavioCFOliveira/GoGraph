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

// hashFloatBits returns the equivalence-consistent hash for the canonical
// (non-NaN) float64 representation f. Shared by the FloatValue, IntegerValue,
// NodeValue, RelationshipValue, and *LazyNodeValue branches of
// [EquivalentHash]: [Value.Equal] compares a cross-type Integer/Float pair
// (and, symmetrically, an Integer against a node/relationship's raw ID) via
// float64(a) == float64(b), so every one of these types must hash through
// this exact same float64 domain to stay consistent — whatever precision the
// comparison tolerates, the hash must tolerate identically.
//
// # Hash-quality trade-off above 2^53 is unavoidable, not a shortcut
//
// float64 has a 53-bit mantissa, so two distinct int64/uint64 values beyond
// 2^53 can round to the identical float64 bit pattern (e.g. 2^53+1 and 2^53
// both round to 2^53) — and [Value.Equal] itself, via the float64(a)==
// float64(b) comparison above, ALREADY treats such a pair as equal. Any hash
// function kept consistent with that comparison (the [EquivalentHash]
// contract this whole file exists to satisfy) MUST therefore also collapse
// that pair to the same hash — there is no lossless-above-2^53 hash domain
// that stays consistent with a comparison that is itself lossy there. The
// alternative (routing IntegerValue/NodeValue/etc. through their own
// lossless raw-bit hash instead) was tried independently in
// cypher/exec/hash_join.go's canonicalKeyHash and produced exactly the
// opposite defect: a hash that DISAGREED with Equal for such a pair, so a
// HashJoin silently dropped a matching row instead of erroring (rmp #1865,
// fixed by making canonicalKeyHash delegate to EquivalentHash instead of
// reimplementing this fold independently).
//
// The measurable consequence is bounded, not unbounded: DISTINCT/GROUP BY
// over adjacent integers beyond 2^53 degrades to a longer collision chain
// (measured 7.7-10.9x slower for that specific access pattern, chain length
// bounded by the number of distinct float64 bit patterns reachable in the
// probed range — roughly 1024-2048 for adjacent-integer inputs, 2026-07-02
// production-readiness audit round 2) rather than the correctness this
// function exists to preserve. The package-cypher aggregate-DISTINCT value
// cap and [exec.DefaultMaxDistinct]/[exec.DefaultMaxGroups] independently
// bound the absolute amount of work any single query can force regardless
// of this collision behaviour, so the trade-off is a measured, cited,
// structurally bounded performance characteristic — not a data-safety or
// DoS concern —
// and changing it would reopen the exact inconsistency this file's own
// [Equivalent]/[EquivalentHash] split exists to close.
func hashFloatBits(f float64) uint64 {
	// Canonicalise -0.0 → +0.0 so both map to the same hash.
	// (IEEE 754: -0.0 == +0.0, so they must be equivalent.)
	if f == 0 {
		f = 0.0 // force positive zero bit pattern
	}
	bits := math.Float64bits(f)
	return bits ^ (bits >> 32)
}

// EquivalentHash returns a hash for v that is consistent with [Equivalent]:
// two values that are Equivalent always produce the same EquivalentHash.
//
// This differs from v.Hash() in three cases:
//   - All NaN bit-patterns map to one canonical hash (NaN ≡ NaN).
//   - -0.0 maps to the same hash as 0.0 (−0.0 == 0.0 in IEEE 754).
//   - IntegerValue, [NodeValue], [*LazyNodeValue], and [RelationshipValue]
//     all hash through the same float64 domain as FloatValue (see
//     [hashFloatBits]), so any pair of these that [Value.Equal] treats as
//     equal — an Integer and a numerically-equal Float, or a node/
//     relationship and an IntegerValue carrying its raw ID (the in-pipeline
//     encoding NodeScan/Expand emit, per [NodeValue.Equal] /
//     [RelationshipValue.Equal] / [LazyNodeValue.Equal]) — always lands in
//     the same hash bucket. Each of their own [Value.Hash] methods folds
//     raw ID/int64 bits directly, unrelated to the IEEE-754 float64 bit
//     pattern, which is exactly why DISTINCT/grouping must call
//     EquivalentHash and never call Hash directly on any of these types.
//     A NodeValue/RelationshipValue ID exceeding 2^53 hashes lossily (the
//     same accepted, bounded hash-quality trade-off IntegerValue already
//     has above that threshold — see hashFloatBits) rather than producing a
//     wrong equivalence result: a hash collision only costs a linear
//     collision-chain comparison, resolved exactly by [Equivalent].
func EquivalentHash(v Value) uint64 {
	if fv, ok := v.(FloatValue); ok {
		f := float64(fv)
		if math.IsNaN(f) {
			// Canonical NaN hash: fixed non-zero constant so all NaN
			// bit-patterns land in the same bucket.
			const nanHash uint64 = 0x7FF8000000000001 // canonical qNaN bits
			return nanHash ^ (nanHash >> 32)
		}
		return hashFloatBits(f)
	}
	if iv, ok := v.(IntegerValue); ok {
		return hashFloatBits(float64(iv))
	}
	if nv, ok := v.(NodeValue); ok {
		return hashFloatBits(float64(nv.ID))
	}
	if lnv, ok := v.(*LazyNodeValue); ok {
		return hashFloatBits(float64(lnv.ID()))
	}
	if rv, ok := v.(RelationshipValue); ok {
		return hashFloatBits(float64(rv.ID))
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
