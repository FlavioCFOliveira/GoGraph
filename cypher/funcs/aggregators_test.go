package funcs_test

// aggregators_test.go — unit tests for the full aggregator function library
// (task-252).
//
// Coverage: Count, CountStar, Sum, Avg, Min, Max, Collect, StdDev, StdDevP,
// PercentileCont, PercentileDisc — correct values, NULL handling, type promotion.

import (
	"math"
	"testing"

	"gograph/cypher/expr"
	"gograph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper
// ─────────────────────────────────────────────────────────────────────────────

func newAgg(factory funcs.AggregatorFactory) funcs.Aggregator {
	return factory()
}

func feedAll(agg funcs.Aggregator, vals ...expr.Value) expr.Value {
	agg.Init()
	for _, v := range vals {
		agg.Step(v)
	}
	return agg.Result()
}

func approxEq(t *testing.T, name string, got expr.Value, want, eps float64) {
	t.Helper()
	f, ok := got.(expr.FloatValue)
	if !ok {
		t.Fatalf("%s: got type %T (%v), want FloatValue", name, got, got)
	}
	if math.Abs(float64(f)-want) > eps {
		t.Errorf("%s: got %g, want %g (eps=%g)", name, float64(f), want, eps)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CountAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestCountAgg(t *testing.T) {
	tests := []struct {
		name string
		vals []expr.Value
		want int64
	}{
		{"empty", nil, 0},
		{"all-null", []expr.Value{expr.Null, expr.Null}, 0},
		{"no-null", []expr.Value{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}, 3},
		{"mixed-null", []expr.Value{expr.Null, expr.IntegerValue(1), expr.Null, expr.IntegerValue(2)}, 2},
		{"single-non-null", []expr.Value{expr.StringValue("x")}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := feedAll(newAgg(funcs.NewCountAgg()), tc.vals...)
			got, ok := result.(expr.IntegerValue)
			if !ok {
				t.Fatalf("got %T, want IntegerValue", result)
			}
			if int64(got) != tc.want {
				t.Errorf("got %d, want %d", int64(got), tc.want)
			}
		})
	}
}

func TestCountAgg_InitReset(t *testing.T) {
	agg := newAgg(funcs.NewCountAgg())
	feedAll(agg, expr.IntegerValue(1), expr.IntegerValue(2))
	// Re-init and feed again.
	agg.Init()
	agg.Step(expr.IntegerValue(9))
	if result := agg.Result(); result != expr.IntegerValue(1) {
		t.Errorf("after re-init: got %v, want 1", result)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CountStarAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestCountStarAgg(t *testing.T) {
	tests := []struct {
		name string
		vals []expr.Value
		want int64
	}{
		{"empty", nil, 0},
		{"counts-nulls", []expr.Value{expr.Null, expr.Null, expr.Null}, 3},
		{"mixed", []expr.Value{expr.Null, expr.IntegerValue(1)}, 2},
		{"five-rows", []expr.Value{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3), expr.IntegerValue(4), expr.IntegerValue(5)}, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := feedAll(newAgg(funcs.NewCountStarAgg()), tc.vals...)
			got, ok := result.(expr.IntegerValue)
			if !ok {
				t.Fatalf("got %T, want IntegerValue", result)
			}
			if int64(got) != tc.want {
				t.Errorf("got %d, want %d", int64(got), tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SumAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestSumAgg(t *testing.T) {
	t.Run("integers", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()),
			expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3))
		if result != expr.IntegerValue(6) {
			t.Errorf("got %v, want 6", result)
		}
	})
	t.Run("floats", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()),
			expr.FloatValue(1.5), expr.FloatValue(2.5))
		approxEq(t, "sum-floats", result, 4.0, 1e-12)
	})
	t.Run("mixed-int-float-promotes", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()),
			expr.IntegerValue(1), expr.FloatValue(0.5))
		approxEq(t, "sum-mixed", result, 1.5, 1e-12)
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()),
			expr.Null, expr.IntegerValue(4), expr.Null)
		if result != expr.IntegerValue(4) {
			t.Errorf("got %v, want 4", result)
		}
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()), expr.Null, expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("empty-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewSumAgg()))
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AvgAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestAvgAgg(t *testing.T) {
	t.Run("integers", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewAvgAgg()),
			expr.IntegerValue(2), expr.IntegerValue(4), expr.IntegerValue(6))
		approxEq(t, "avg-int", result, 4.0, 1e-12)
	})
	t.Run("floats", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewAvgAgg()),
			expr.FloatValue(1.0), expr.FloatValue(3.0))
		approxEq(t, "avg-float", result, 2.0, 1e-12)
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewAvgAgg()),
			expr.Null, expr.IntegerValue(5), expr.IntegerValue(15))
		approxEq(t, "avg-skip-null", result, 10.0, 1e-12)
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewAvgAgg()), expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("empty-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewAvgAgg()))
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// MinAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestMinAgg(t *testing.T) {
	t.Run("integers", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMinAgg()),
			expr.IntegerValue(5), expr.IntegerValue(2), expr.IntegerValue(8))
		if result != expr.IntegerValue(2) {
			t.Errorf("got %v, want 2", result)
		}
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMinAgg()),
			expr.Null, expr.IntegerValue(3), expr.IntegerValue(1))
		if result != expr.IntegerValue(1) {
			t.Errorf("got %v, want 1", result)
		}
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMinAgg()), expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("strings", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMinAgg()),
			expr.StringValue("banana"), expr.StringValue("apple"), expr.StringValue("cherry"))
		if result != expr.StringValue("apple") {
			t.Errorf("got %v, want \"apple\"", result)
		}
	})
	t.Run("single-value", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMinAgg()), expr.IntegerValue(42))
		if result != expr.IntegerValue(42) {
			t.Errorf("got %v, want 42", result)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// MaxAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestMaxAgg(t *testing.T) {
	t.Run("integers", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMaxAgg()),
			expr.IntegerValue(5), expr.IntegerValue(2), expr.IntegerValue(8))
		if result != expr.IntegerValue(8) {
			t.Errorf("got %v, want 8", result)
		}
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMaxAgg()),
			expr.Null, expr.IntegerValue(3), expr.IntegerValue(9))
		if result != expr.IntegerValue(9) {
			t.Errorf("got %v, want 9", result)
		}
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMaxAgg()), expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("strings", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewMaxAgg()),
			expr.StringValue("banana"), expr.StringValue("apple"), expr.StringValue("cherry"))
		if result != expr.StringValue("cherry") {
			t.Errorf("got %v, want \"cherry\"", result)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// CollectAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestCollectAgg(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewCollectAgg()))
		lv, ok := result.(expr.ListValue)
		if !ok {
			t.Fatalf("got %T, want ListValue", result)
		}
		if len(lv) != 0 {
			t.Errorf("got %v, want []", lv)
		}
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewCollectAgg()),
			expr.Null, expr.IntegerValue(1), expr.Null, expr.IntegerValue(2))
		lv, ok := result.(expr.ListValue)
		if !ok {
			t.Fatalf("got %T, want ListValue", result)
		}
		if len(lv) != 2 {
			t.Fatalf("len=%d, want 2", len(lv))
		}
		if lv[0] != expr.IntegerValue(1) || lv[1] != expr.IntegerValue(2) {
			t.Errorf("got %v, want [1, 2]", lv)
		}
	})
	t.Run("ordering-preserved", func(t *testing.T) {
		vals := []expr.Value{
			expr.StringValue("c"), expr.StringValue("a"), expr.StringValue("b"),
		}
		result := feedAll(newAgg(funcs.NewCollectAgg()), vals...)
		lv := result.(expr.ListValue) //nolint:forcetypeassert // test; type asserted above
		for i, want := range vals {
			if lv[i] != want {
				t.Errorf("lv[%d] = %v, want %v", i, lv[i], want)
			}
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// StdDevAgg (sample)
// ─────────────────────────────────────────────────────────────────────────────

func TestStdDevAgg(t *testing.T) {
	t.Run("known-values", func(t *testing.T) {
		// [2, 4, 4, 4, 5, 5, 7, 9] — sample std dev ≈ 2.138 (sqrt(32/7))
		result := feedAll(newAgg(funcs.NewStdDevAgg()),
			expr.FloatValue(2), expr.FloatValue(4), expr.FloatValue(4),
			expr.FloatValue(4), expr.FloatValue(5), expr.FloatValue(5),
			expr.FloatValue(7), expr.FloatValue(9))
		approxEq(t, "stdev", result, math.Sqrt(32.0/7.0), 1e-9)
	})
	t.Run("single-value-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewStdDevAgg()), expr.FloatValue(5.0))
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL (sample stdev undefined for n=1)", result)
		}
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewStdDevAgg()), expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("skip-null-in-mix", func(t *testing.T) {
		// Same as known-values but with NULLs sprinkled in — result unchanged.
		result := feedAll(newAgg(funcs.NewStdDevAgg()),
			expr.Null,
			expr.FloatValue(2), expr.FloatValue(4), expr.FloatValue(4),
			expr.Null,
			expr.FloatValue(4), expr.FloatValue(5), expr.FloatValue(5),
			expr.FloatValue(7), expr.FloatValue(9),
			expr.Null)
		approxEq(t, "stdev-with-nulls", result, math.Sqrt(32.0/7.0), 1e-9)
	})
	t.Run("two-identical-values", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewStdDevAgg()),
			expr.FloatValue(3.0), expr.FloatValue(3.0))
		approxEq(t, "stdev-identical", result, 0.0, 1e-12)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// StdDevPAgg (population)
// ─────────────────────────────────────────────────────────────────────────────

func TestStdDevPAgg(t *testing.T) {
	t.Run("known-values", func(t *testing.T) {
		// [2, 4, 4, 4, 5, 5, 7, 9] — population std dev = sqrt(32/8) = 2.0
		result := feedAll(newAgg(funcs.NewStdDevPAgg()),
			expr.FloatValue(2), expr.FloatValue(4), expr.FloatValue(4),
			expr.FloatValue(4), expr.FloatValue(5), expr.FloatValue(5),
			expr.FloatValue(7), expr.FloatValue(9))
		approxEq(t, "stdevp", result, 2.0, 1e-9)
	})
	t.Run("single-value-zero", func(t *testing.T) {
		// Population stdev of a single value is 0 (no spread).
		result := feedAll(newAgg(funcs.NewStdDevPAgg()), expr.FloatValue(5.0))
		approxEq(t, "stdevp-single", result, 0.0, 1e-12)
	})
	t.Run("all-null-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewStdDevPAgg()), expr.Null)
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// PercentileContAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestPercentileContAgg(t *testing.T) {
	vals := func(is ...float64) []expr.Value {
		out := make([]expr.Value, len(is))
		for i, v := range is {
			out[i] = expr.FloatValue(v)
		}
		return out
	}

	t.Run("p0", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(0.0)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "p0", result, 1.0, 1e-12)
	})
	t.Run("p1", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(1.0)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "p1", result, 5.0, 1e-12)
	})
	t.Run("p0.5-median-odd", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(0.5)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "median-odd", result, 3.0, 1e-12)
	})
	t.Run("p0.5-median-even", func(t *testing.T) {
		// [1, 2, 3, 4] → median = 2.5
		result := feedAll(newAgg(funcs.NewPercentileContAgg(0.5)), vals(1, 2, 3, 4)...)
		approxEq(t, "median-even", result, 2.5, 1e-12)
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(0.5)),
			expr.Null, expr.FloatValue(2), expr.FloatValue(4))
		approxEq(t, "skip-null", result, 3.0, 1e-12)
	})
	t.Run("empty-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(0.5)))
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("clamp-above-1", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileContAgg(2.0)), vals(1, 2, 3)...)
		approxEq(t, "clamp-above-1", result, 3.0, 1e-12)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// PercentileDiscAgg
// ─────────────────────────────────────────────────────────────────────────────

func TestPercentileDiscAgg(t *testing.T) {
	vals := func(is ...float64) []expr.Value {
		out := make([]expr.Value, len(is))
		for i, v := range is {
			out[i] = expr.FloatValue(v)
		}
		return out
	}

	t.Run("p0", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileDiscAgg(0.0)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "p0", result, 1.0, 1e-12)
	})
	t.Run("p1", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileDiscAgg(1.0)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "p1", result, 5.0, 1e-12)
	})
	t.Run("p0.5-odd", func(t *testing.T) {
		// [1,2,3,4,5] → disc 0.5 → ceil(0.5*5)=3 → index 2 → value 3
		result := feedAll(newAgg(funcs.NewPercentileDiscAgg(0.5)), vals(1, 2, 3, 4, 5)...)
		approxEq(t, "p0.5-odd", result, 3.0, 1e-12)
	})
	t.Run("empty-returns-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileDiscAgg(0.5)))
		if !expr.IsNull(result) {
			t.Errorf("got %v, want NULL", result)
		}
	})
	t.Run("skip-null", func(t *testing.T) {
		result := feedAll(newAgg(funcs.NewPercentileDiscAgg(0.0)),
			expr.Null, expr.FloatValue(5), expr.FloatValue(10))
		approxEq(t, "skip-null", result, 5.0, 1e-12)
	})
}
