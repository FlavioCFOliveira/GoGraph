package cypher

// distinct_aggregator_internal_test.go — regression coverage for rmp #1867
// (2026-07-02 production-readiness audit round 2, finding "distinctAggregator
// has no memory cap").
//
// Background. distinctAggregator's seen-values set (the dedup bookkeeping for
// count/sum/avg/min/max/collect(DISTINCT ...)) had neither a count cap nor a
// byte budget, unlike every sibling pipeline-breaker (exec.Distinct,
// exec.EagerAggregation, funcs.CollectAgg). A streaming aggregator such as
// count(DISTINCT x) held O(1) state of its own, so the seen-values set was the
// ONLY thing growing per distinct value — with no bound at all.
//
// White-box (package cypher, not cypher_test) so these tests can directly
// override the count cap via newDistinctAggregator's maxValues parameter and
// the byte dimension via WithByteBudget, exactly like exec.NewDistinct's own
// tests do (see cypher/exec/pipeline_ops_test.go TestDistinct_MemoryCapEnforced),
// instead of needing millions of Step calls to reach the real 10-million
// default.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// TestDistinctAggregator_CountCapEnforced verifies Step returns
// ErrAggregateDistinctMemoryExceeded once more than maxValues distinct
// values have been accepted, and that the cap is a hard stop (a further
// distinct value keeps erroring, not just the one that crossed the line).
func TestDistinctAggregator_CountCapEnforced(t *testing.T) {
	d := newDistinctAggregator(funcs.NewCountAgg()(), 3)
	d.Init()

	for i := int64(0); i < 3; i++ {
		if err := d.Step(expr.IntegerValue(i)); err != nil {
			t.Fatalf("Step(%d): unexpected error: %v", i, err)
		}
	}
	if err := d.Step(expr.IntegerValue(3)); !errors.Is(err, ErrAggregateDistinctMemoryExceeded) {
		t.Fatalf("Step(3): got %v, want ErrAggregateDistinctMemoryExceeded", err)
	}
	// The cap is a hard stop, not a one-shot trip.
	if err := d.Step(expr.IntegerValue(4)); !errors.Is(err, ErrAggregateDistinctMemoryExceeded) {
		t.Fatalf("Step(4) after cap: got %v, want ErrAggregateDistinctMemoryExceeded", err)
	}
}

// TestDistinctAggregator_CountCapIgnoresDuplicates verifies re-stepping an
// ALREADY-SEEN value never consumes cap capacity — only genuinely new
// distinct values do. With maxValues=1, one hundred repeats of the SAME
// value must all succeed (a single distinct value fits the cap); only a
// SECOND, genuinely distinct value should trip it.
func TestDistinctAggregator_CountCapIgnoresDuplicates(t *testing.T) {
	d := newDistinctAggregator(funcs.NewCountAgg()(), 1)
	d.Init()

	for i := 0; i < 100; i++ {
		if err := d.Step(expr.IntegerValue(1)); err != nil {
			t.Fatalf("Step(1) iteration %d: unexpected error (duplicates must not consume cap capacity): %v", i, err)
		}
	}
	if got := d.Result(); got != expr.IntegerValue(1) {
		t.Fatalf("count(DISTINCT) after 100 duplicate steps = %v, want 1", got)
	}
	if err := d.Step(expr.IntegerValue(2)); !errors.Is(err, ErrAggregateDistinctMemoryExceeded) {
		t.Fatalf("Step(2) (a genuinely new distinct value at cap=1): got %v, want ErrAggregateDistinctMemoryExceeded", err)
	}
}

// TestDistinctAggregator_ByteBudgetEnforced verifies the byte dimension trips
// independently of the count cap when large-valued distinct inputs would
// exceed it well before maxValues is reached.
func TestDistinctAggregator_ByteBudgetEnforced(t *testing.T) {
	estimate := func(v expr.Value) int64 {
		s, ok := v.(expr.StringValue)
		if !ok {
			return 1
		}
		return int64(len(s))
	}
	// Cap large enough for one ~50-byte string but not two.
	d := newDistinctAggregator(funcs.NewCountAgg()(), 1_000_000).WithByteBudget(50, estimate)
	d.Init()

	first := expr.StringValue("0123456789012345678901234567890123456789") // 40 bytes
	if err := d.Step(first); err != nil {
		t.Fatalf("Step(first): unexpected error: %v", err)
	}
	second := expr.StringValue("9876543210987654321098765432109876543210") // another 40 bytes, distinct value
	if err := d.Step(second); !errors.Is(err, ErrAggregateDistinctMemoryExceeded) {
		t.Fatalf("Step(second): got %v, want ErrAggregateDistinctMemoryExceeded (40+40=80 > 50 byte budget)", err)
	}
}

// TestDistinctAggregator_ByteBudgetDisabledByDefault verifies a zero maxBytes
// (or a nil estimator) leaves the byte dimension off, matching byteBudget's
// own disabled-by-default convention — only the count cap applies.
func TestDistinctAggregator_ByteBudgetDisabledByDefault(t *testing.T) {
	d := newDistinctAggregator(funcs.NewCountAgg()(), 0)
	d.Init()
	for i := int64(0); i < 1000; i++ {
		if err := d.Step(expr.IntegerValue(i)); err != nil {
			t.Fatalf("Step(%d): unexpected error with byte dimension disabled: %v", i, err)
		}
	}
}

// TestDistinctAggregator_ZeroMaxValuesUsesDefault verifies the zero-means-
// default convention mirrors exec.NewDistinct's.
func TestDistinctAggregator_ZeroMaxValuesUsesDefault(t *testing.T) {
	d := newDistinctAggregator(funcs.NewCountAgg()(), 0)
	if d.maxValues != DefaultMaxAggregateDistinctValues {
		t.Fatalf("maxValues = %d, want DefaultMaxAggregateDistinctValues (%d)", d.maxValues, DefaultMaxAggregateDistinctValues)
	}
}
