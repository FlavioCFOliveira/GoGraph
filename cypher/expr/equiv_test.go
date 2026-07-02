package expr

// equiv_test.go — regression gate for the 2026-07-02 production-readiness
// audit finding F2: EquivalentHash(IntegerValue) and EquivalentHash(FloatValue)
// disagreed for a numerically-equal pair (Equivalent(1, 1.0) == true per
// IntegerValue.Equal/FloatValue.Equal, but their hashes landed in different
// buckets), breaking the invariant documented on EquivalentHash and causing
// DISTINCT/grouping/UNION to over-count whenever the same logical number
// appeared as both an INTEGER and a FLOAT.

import (
	"math"
	"testing"
)

// TestEquivalentHash_IntFloatInvariant is the direct repro: for every pair the
// audit found broken, Equivalent(a,b) must imply EquivalentHash(a)==EquivalentHash(b).
func TestEquivalentHash_IntFloatInvariant(t *testing.T) {
	cases := []struct {
		name string
		a, b Value
	}{
		{"1 vs 1.0", IntegerValue(1), FloatValue(1.0)},
		{"1.0 vs 1 (reversed)", FloatValue(1.0), IntegerValue(1)},
		{"0 vs 0.0", IntegerValue(0), FloatValue(0.0)},
		{"0 vs -0.0", IntegerValue(0), FloatValue(math.Copysign(0, -1))},
		{"-5 vs -5.0", IntegerValue(-5), FloatValue(-5.0)},
		{"large safe int vs equal float", IntegerValue(1 << 52), FloatValue(float64(int64(1) << 52))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !Equivalent(tc.a, tc.b) {
				t.Fatalf("precondition failed: Equivalent(%v, %v) = false, want true", tc.a, tc.b)
			}
			ha, hb := EquivalentHash(tc.a), EquivalentHash(tc.b)
			if ha != hb {
				t.Errorf("Equivalent(%v, %v) is true but EquivalentHash disagrees: %d != %d", tc.a, tc.b, ha, hb)
			}
		})
	}
}

// TestEquivalentHash_IntFloat_NotCollapsedForDistinctValues is a negative
// control: the fix must not make every Integer/Float pair hash identically —
// only numerically-equal ones.
func TestEquivalentHash_IntFloat_NotCollapsedForDistinctValues(t *testing.T) {
	cases := []struct {
		name string
		a, b Value
	}{
		{"1 vs 2", IntegerValue(1), IntegerValue(2)},
		{"1 vs 2.0", IntegerValue(1), FloatValue(2.0)},
		{"1 vs 1.5", IntegerValue(1), FloatValue(1.5)},
		{"1.0 vs 1.5", FloatValue(1.0), FloatValue(1.5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if Equivalent(tc.a, tc.b) {
				t.Fatalf("precondition failed: Equivalent(%v, %v) = true, want false", tc.a, tc.b)
			}
			if EquivalentHash(tc.a) == EquivalentHash(tc.b) {
				t.Errorf("distinct values %v and %v collided at hash %d", tc.a, tc.b, EquivalentHash(tc.a))
			}
		})
	}
}

// TestEquivalentHash_ListWithIntFloatMix proves the fix propagates through
// ListValue's recursive hashing: two lists that differ only in whether an
// element is stored as an Integer or an equal Float must hash equal.
func TestEquivalentHash_ListWithIntFloatMix(t *testing.T) {
	a := ListValue{IntegerValue(1), StringValue("x"), IntegerValue(2)}
	b := ListValue{FloatValue(1.0), StringValue("x"), FloatValue(2.0)}
	if !Equivalent(a, b) {
		t.Fatalf("precondition failed: Equivalent(%v, %v) = false, want true", a, b)
	}
	if EquivalentHash(a) != EquivalentHash(b) {
		t.Errorf("EquivalentHash(%v)=%d != EquivalentHash(%v)=%d", a, EquivalentHash(a), b, EquivalentHash(b))
	}
}

// TestEquivalentHash_FloatNaNAndZero pins the pre-existing FloatValue
// canonicalisation (NaN, -0.0) still holds after routing FloatValue's hash
// through the shared hashFloatBits helper.
func TestEquivalentHash_FloatNaNAndZero(t *testing.T) {
	nan1 := FloatValue(math.NaN())
	nan2 := FloatValue(math.Float64frombits(0x7FF8000000000002)) // a different NaN bit pattern
	if EquivalentHash(nan1) != EquivalentHash(nan2) {
		t.Errorf("all NaN bit-patterns must hash identically: %d != %d", EquivalentHash(nan1), EquivalentHash(nan2))
	}
	posZero := FloatValue(0.0)
	negZero := FloatValue(math.Copysign(0, -1))
	if EquivalentHash(posZero) != EquivalentHash(negZero) {
		t.Errorf("+0.0 and -0.0 must hash identically: %d != %d", EquivalentHash(posZero), EquivalentHash(negZero))
	}
}
