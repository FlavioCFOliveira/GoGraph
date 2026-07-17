package expr_test

// equiv_exact_test.go — rmp #2050
//
// Regression gate for the exact cross-type INTEGER↔FLOAT equality mandated by
// openCypher CIP2016-06-14 ("coerced to unlimited-precision … compared
// numerically in their natural order"). Before the fix, both the `=` path
// ([Value.Equal]) and grouping/DISTINCT equivalence ([Equivalent]) compared a
// cross-type Integer/Float pair via lossy float64 promotion
// (float64(a) == float64(b)), which wrongly equated an integer no float64 can
// represent — e.g. 2^53+1 — with the nearest float (2^53.0).
//
// These tests pin, at the value layer that every consumer (WHERE `=`,
// list/map element equality, DISTINCT, grouping, HashJoin) inherits from:
//   - the fix: int 2^53+1 ≠ float 2^53.0 for both Equal and Equivalent;
//   - the still-equal representable cases (1 = 1.0, 2^53 = 2^53.0) stay true;
//   - NaN / ±Inf / fractional floats never equal an integer;
//   - list equality [2^53+1] = [2^53.0] is false, [2^53] = [2^53.0] is true;
//   - the EquivalentHash consistency invariant "a Equivalent b ⇒ same hash"
//     still holds, and the now-unequal 2^53+1 / 2^53.0 pair is a benign
//     same-bucket collision resolved by the exact comparator.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

const (
	twoPow53   int64   = 1 << 53          // 9007199254740992, exactly representable as float64
	twoPow53p1 int64   = (1 << 53) + 1    // 9007199254740993, NOT representable as float64
	twoPow53p2 int64   = (1 << 53) + 2    // 9007199254740994, NOT representable as float64
	twoPow53f  float64 = float64(1 << 53) // 9007199254740992.0
)

// crossCase is a single int-vs-float equality expectation.
type crossCase struct {
	name string
	i    int64
	f    float64
	want bool
}

func crossCases() []crossCase {
	return []crossCase{
		{"1 == 1.0", 1, 1.0, true},
		{"2 == 2.0", 2, 2.0, true},
		{"0 == 0.0", 0, 0.0, true},
		{"0 == -0.0", 0, math.Copysign(0, -1), true},
		{"-1 == -1.0", -1, -1.0, true},
		{"2 != 2.5", 2, 2.5, false},
		{"3 != 3.0000000001", 3, 3.0000000001, false},
		// The exactness fix: 2^53 is representable (equal), 2^53+1 is not.
		{"2^53 == 2^53.0", twoPow53, twoPow53f, true},
		{"2^53+1 != 2^53.0", twoPow53p1, twoPow53f, false},
		{"2^53+2 != 2^53.0", twoPow53p2, twoPow53f, false},
		// float64(2^53+1) rounds to 2^53.0, so the "obvious" float of that int
		// is still a distinct number from the int.
		{"2^53+1 != float64(2^53+1)", twoPow53p1, float64(twoPow53p1), false},
		// Whole float outside int64 range: float64(MaxInt64) rounds up to 2^63,
		// which no int64 can name.
		{"MaxInt64 != float64(MaxInt64)", math.MaxInt64, float64(math.MaxInt64), false},
		// MinInt64 (−2^63) IS exactly representable as float64.
		{"MinInt64 == -2^63.0", math.MinInt64, math.MinInt64, true},
		// Non-finite floats are never equal to any integer.
		{"5 != NaN", 5, math.NaN(), false},
		{"0 != NaN", 0, math.NaN(), false},
		{"5 != +Inf", 5, math.Inf(1), false},
		{"5 != -Inf", 5, math.Inf(-1), false},
	}
}

// TestValueEqual_CrossTypeExact drives the `=` / WHERE path ([Value.Equal]) in
// BOTH operand orders and asserts the result is exact and symmetric.
func TestValueEqual_CrossTypeExact(t *testing.T) {
	for _, c := range crossCases() {
		iv := expr.IntegerValue(c.i)
		fv := expr.FloatValue(c.f)

		if got := expr.IsTruthy(iv.Equal(fv)); got != c.want {
			t.Errorf("%s: IntegerValue.Equal(FloatValue) = %v, want %v", c.name, got, c.want)
		}
		if got := expr.IsTruthy(fv.Equal(iv)); got != c.want {
			t.Errorf("%s: FloatValue.Equal(IntegerValue) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEquivalent_CrossTypeExact drives the grouping/DISTINCT equivalence path.
// For non-NaN operands it must agree with Value.Equal on int↔float exactness.
func TestEquivalent_CrossTypeExact(t *testing.T) {
	for _, c := range crossCases() {
		iv := expr.IntegerValue(c.i)
		fv := expr.FloatValue(c.f)

		// A NaN float is never equivalent to a finite integer (NaN ≡ NaN only,
		// and an integer is never NaN); every other case must match Value.Equal.
		want := c.want
		if math.IsNaN(c.f) {
			want = false
		}
		if got := expr.Equivalent(iv, fv); got != want {
			t.Errorf("%s: Equivalent(int, float) = %v, want %v", c.name, got, want)
		}
		if got := expr.Equivalent(fv, iv); got != want {
			t.Errorf("%s: Equivalent(float, int) = %v, want %v", c.name, got, want)
		}
	}
}

// TestListEqual_CrossTypeExact pins list element equality (recurses into
// Value.Equal): [2^53+1] = [2^53.0] is false, [2^53] = [2^53.0] is true.
func TestListEqual_CrossTypeExact(t *testing.T) {
	eq := func(a, b expr.Value) expr.Value {
		return expr.ListValue{a}.Equal(expr.ListValue{b})
	}
	if got := expr.IsTruthy(eq(expr.IntegerValue(twoPow53p1), expr.FloatValue(twoPow53f))); got {
		t.Errorf("[2^53+1] = [2^53.0]: got true, want false")
	}
	if got := expr.IsTruthy(eq(expr.IntegerValue(twoPow53), expr.FloatValue(twoPow53f))); !got {
		t.Errorf("[2^53] = [2^53.0]: got false, want true")
	}
	if got := expr.IsTruthy(eq(expr.IntegerValue(1), expr.FloatValue(1.0))); !got {
		t.Errorf("[1] = [1.0]: got false, want true")
	}
}

// TestEquivalentHash_ConsistencyInvariant proves the bucketing invariant
// "a Equivalent b ⇒ EquivalentHash(a) == EquivalentHash(b)" STILL holds after
// the exactness fix: every genuinely-equivalent pair shares a hash bucket.
func TestEquivalentHash_ConsistencyInvariant(t *testing.T) {
	type pair struct {
		name string
		a, b expr.Value
	}
	equalPairs := []pair{
		{"1 / 1.0", expr.IntegerValue(1), expr.FloatValue(1.0)},
		{"0 / 0.0", expr.IntegerValue(0), expr.FloatValue(0.0)},
		{"0 / -0.0", expr.IntegerValue(0), expr.FloatValue(math.Copysign(0, -1))},
		{"-1 / -1.0", expr.IntegerValue(-1), expr.FloatValue(-1.0)},
		{"2^53 / 2^53.0", expr.IntegerValue(twoPow53), expr.FloatValue(twoPow53f)},
		{"MinInt64 / -2^63.0", expr.IntegerValue(math.MinInt64), expr.FloatValue(math.MinInt64)},
		// Node/relationship identity against an Integer carrying the raw ID also
		// routes through the shared float64-image bucket.
		{"node#7 / int 7", expr.NodeValue{ID: 7}, expr.IntegerValue(7)},
		{"rel#7 / int 7", expr.RelationshipValue{ID: 7}, expr.IntegerValue(7)},
	}
	for _, p := range equalPairs {
		// Precondition: the pair really is equivalent.
		if !expr.Equivalent(p.a, p.b) {
			t.Fatalf("%s: expected Equivalent, got not-equivalent (test premise broken)", p.name)
		}
		if ha, hb := expr.EquivalentHash(p.a), expr.EquivalentHash(p.b); ha != hb {
			t.Errorf("%s: EquivalentHash mismatch %#x vs %#x — invariant broken", p.name, ha, hb)
		}
	}

	// Benign collision: 2^53+1 and 2^53.0 are NOT equivalent (the fix) yet share
	// a hash bucket (both fold through 2^53.0's float64 bits). This is correct —
	// the comparator resolves the collision — and documents that the hash did
	// NOT need to change.
	a := expr.IntegerValue(twoPow53p1)
	b := expr.FloatValue(twoPow53f)
	if expr.Equivalent(a, b) {
		t.Errorf("2^53+1 / 2^53.0: got Equivalent, want NOT equivalent")
	}
	if expr.EquivalentHash(a) != expr.EquivalentHash(b) {
		t.Errorf("2^53+1 / 2^53.0: expected same (benign) hash bucket, got different")
	}
}
