package exec

// constraints_numeric_equiv_test.go — regression for #1910: UNIQUE value
// identity uses value-equivalence, so numerically-equal int and float share one
// key (aligning with openCypher = and MERGE #1240). Exact-value based, hence
// transitive: two integers beyond 2^53 that round to the same float64 are NOT
// folded.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestPropertyValueToString_NumericValueEquivalence(t *testing.T) {
	key := func(v lpg.PropertyValue) string {
		k, ok := propertyValueToString(v)
		if !ok {
			t.Fatalf("expected a key for %v", v)
		}
		return k
	}

	// int 1 ≡ float 1.0.
	if key(lpg.Int64Value(1)) != key(lpg.Float64Value(1.0)) {
		t.Error("int 1 and float 1.0 must share a UNIQUE key")
	}
	// +0.0 ≡ -0.0 ≡ int 0.
	if key(lpg.Float64Value(math.Copysign(0, 1))) != key(lpg.Int64Value(0)) ||
		key(lpg.Float64Value(math.Copysign(0, -1))) != key(lpg.Int64Value(0)) {
		t.Error("+0.0, -0.0 and int 0 must share a UNIQUE key")
	}
	// All NaN collapse to one key.
	if key(lpg.Float64Value(math.NaN())) != key(lpg.Float64Value(math.Float64frombits(0x7FF8000000000001))) {
		t.Error("all NaN must share a UNIQUE key")
	}
	// Distinct kinds/values stay distinct.
	if key(lpg.Int64Value(1)) == key(lpg.StringValue("1")) {
		t.Error("int 1 and string \"1\" must NOT share a key")
	}
	if key(lpg.Float64Value(1.5)) == key(lpg.Int64Value(1)) {
		t.Error("float 1.5 must NOT fold onto an integer")
	}
	// Transitivity guard: 2^53+1 (an integer) must not fold with the float
	// 2^53.0 it would round to — they are different values.
	const twoTo53 = int64(1) << 53
	if key(lpg.Int64Value(twoTo53+1)) == key(lpg.Float64Value(float64(twoTo53))) {
		t.Error("int 2^53+1 must NOT fold onto float 2^53.0 (distinct values)")
	}
	// But an integral float exactly equal to its integer still folds, even large.
	if key(lpg.Int64Value(twoTo53)) != key(lpg.Float64Value(float64(twoTo53))) {
		t.Error("int 2^53 and float 2^53.0 must share a key")
	}
}
