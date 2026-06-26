package expr_test

// compare_numeric_test.go — regression gate for #1789 (sprint 250): Integer and
// Float must be ordered as a SINGLE Number tier (CIP2016-06-14), by magnitude,
// not by kind weight. This unit test pins expr.Compare across the int/float
// boundary, including the ≥2^53 precision edge where float64(int64) is lossy.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestCompareNumericCross_1789(t *testing.T) {
	i := func(n int64) expr.Value { return expr.IntegerValue(n) }
	f := func(x float64) expr.Value { return expr.FloatValue(x) }

	cases := []struct {
		name string
		a, b expr.Value
		want int
	}{
		{"1 < 1.5", i(1), f(1.5), -1},
		{"1.5 > 1", f(1.5), i(1), 1},
		{"2 > 1.5", i(2), f(1.5), 1},
		{"3.0 == 3", f(3.0), i(3), 0},
		{"3 == 3.0", i(3), f(3.0), 0},
		{"0.5 < 1", f(0.5), i(1), -1},
		{"100.0 > 2", f(100.0), i(2), 1},
		{"-1 < 0.0", i(-1), f(0.0), -1},
		{"NaN sorts after int", i(5), f(math.NaN()), -1},
		{"int before NaN (rev)", f(math.NaN()), i(5), 1},
		{"+Inf > maxint64", i(math.MaxInt64), f(math.Inf(1)), -1},
		{"-Inf < minint64", i(math.MinInt64), f(math.Inf(-1)), 1},
		// Precision edge: 2^53+1 is not representable as float64; the int is
		// strictly greater than the float 2^53 (which equals 2^53 exactly).
		{"2^53+1 > 2^53.0", i((1 << 53) + 1), f(math.Pow(2, 53)), 1},
		{"2^53 == 2^53.0", i(1 << 53), f(math.Pow(2, 53)), 0},
		// Large int vs large-but-smaller float.
		{"maxint64 > 1e18", i(math.MaxInt64), f(1e18), 1},
		{"1e18 < maxint64 (rev)", f(1e18), i(math.MaxInt64), -1},
	}
	for _, c := range cases {
		if got := expr.Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare = %d, want %d", c.name, got, c.want)
		}
	}

	// The cross-tier order (String < Bool < Number < Null) must be unchanged.
	if expr.Compare(expr.StringValue("z"), i(0)) != -1 {
		t.Errorf("String must sort before Number")
	}
	if expr.Compare(f(1.0), expr.Null) != -1 {
		t.Errorf("Number must sort before Null")
	}
}
