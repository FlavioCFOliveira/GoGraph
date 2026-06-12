package exec_test

// distinct_equivalence_test.go — integration tests for DISTINCT and grouping
// equivalence semantics (openCypher CIP2016-06-14).
//
// Verified behaviours:
//   - NaN ≡ NaN: DISTINCT collapses two NaN floats into one row
//   - nested null ≡ nested null: DISTINCT collapses [1,null] duplicates
//   - count(DISTINCT x) over [NaN, NaN] returns 1
//   - [null, null] DISTINCT → 1 (regression: must still work)
//   - 0.0 and -0.0 are equivalent (IEEE 754: 0.0 == -0.0)
//   - null ≢ NaN → two distinct rows
//   - [NaN] ≡ [NaN] inside a list
//   - NaN grouping key: two rows with NaN key land in one group
//   - [1,null] grouping key: two rows with [1,null] key land in one group

import (
	"context"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func nanFloat() expr.Value { return expr.FloatValue(math.NaN()) }

// sliceSource is an Operator that emits a fixed slice of rows.
type sliceSource struct {
	rows []exec.Row
	idx  int
}

func (s *sliceSource) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *sliceSource) Close() error                 { return nil }
func (s *sliceSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}

// collectDistinct runs the Distinct operator over single-column rows built from
// vals and returns the number of distinct rows emitted.
func collectDistinct(t *testing.T, vals []expr.Value) int {
	t.Helper()
	rows := make([]exec.Row, len(vals))
	for i, v := range vals {
		rows[i] = exec.Row{v}
	}
	src := &sliceSource{rows: rows}
	op := exec.NewDistinct(src, 0)

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = op.Close() })

	count := 0
	var out exec.Row
	for {
		ok, err := op.Next(&out)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	return count
}

// countViaDist uses Distinct + EagerAggregation(count*) to compute the number
// of distinct values in a single-column input.
func countViaDist(t *testing.T, vals []expr.Value) int64 {
	t.Helper()
	rows := make([]exec.Row, len(vals))
	for i, v := range vals {
		rows[i] = exec.Row{v}
	}
	src := &sliceSource{rows: rows}
	dist := exec.NewDistinct(src, 0)

	agg, err := exec.NewEagerAggregation(
		dist,
		nil, // no group keys — global aggregate
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}

	ctx := context.Background()
	if err := agg.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = agg.Close() })

	var out exec.Row
	ok, err := agg.Next(&out)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !ok {
		t.Fatal("EagerAggregation emitted no rows for non-empty input")
	}
	iv, ok2 := out[0].(expr.IntegerValue)
	if !ok2 {
		t.Fatalf("expected IntegerValue, got %T (%v)", out[0], out[0])
	}
	return int64(iv)
}

// groupCount runs EagerAggregation with col 0 as the group key and a count*
// aggregator over the remaining columns. Returns the number of distinct groups
// emitted.
func groupCount(t *testing.T, rows []exec.Row) int {
	t.Helper()
	src := &sliceSource{rows: rows}
	agg, err := exec.NewEagerAggregation(
		src,
		[]int{0}, // group key: col 0
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
		0,
	)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}

	ctx := context.Background()
	if err := agg.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = agg.Close() })

	count := 0
	var out exec.Row
	for {
		ok, err := agg.Next(&out)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	return count
}

// ─────────────────────────────────────────────────────────────────────────────
// DISTINCT tests
// ─────────────────────────────────────────────────────────────────────────────

// TestDistinct_NaNEquivalence verifies that two NaN floats are treated as
// equivalent and collapsed to a single DISTINCT row (CIP2016-06-14 §393).
//
//	UNWIND [0.0/0.0, 0.0/0.0] AS x RETURN DISTINCT x  → 1 row
func TestDistinct_NaNEquivalence(t *testing.T) {
	got := collectDistinct(t, []expr.Value{nanFloat(), nanFloat()})
	if got != 1 {
		t.Errorf("DISTINCT [NaN, NaN]: got %d rows, want 1", got)
	}
}

// TestDistinct_NestedNullEquivalence verifies that [1, null] ≡ [1, null].
//
//	UNWIND [[1,null],[1,null]] AS x RETURN DISTINCT x  → 1 row
func TestDistinct_NestedNullEquivalence(t *testing.T) {
	list := expr.ListValue{expr.IntegerValue(1), expr.Null}
	got := collectDistinct(t, []expr.Value{list, list})
	if got != 1 {
		t.Errorf("DISTINCT [[1,null],[1,null]]: got %d rows, want 1", got)
	}
}

// TestDistinct_NullNullEquivalence is a regression guard: top-level null
// deduplication worked before; must still work.
//
//	UNWIND [null, null] AS x RETURN DISTINCT x  → 1 row
func TestDistinct_NullNullEquivalence(t *testing.T) {
	got := collectDistinct(t, []expr.Value{expr.Null, expr.Null})
	if got != 1 {
		t.Errorf("DISTINCT [null, null]: got %d rows, want 1", got)
	}
}

// TestDistinct_NegativeZeroEquivalence verifies that 0.0 and -0.0 deduplicate
// (IEEE 754: 0.0 == -0.0).
//
//	UNWIND [0.0, -0.0] AS x RETURN DISTINCT x  → 1 row
func TestDistinct_NegativeZeroEquivalence(t *testing.T) {
	pos := expr.FloatValue(0.0)
	neg := expr.FloatValue(math.Copysign(0, -1))
	got := collectDistinct(t, []expr.Value{pos, neg})
	if got != 1 {
		t.Errorf("DISTINCT [0.0, -0.0]: got %d rows, want 1", got)
	}
}

// TestDistinct_NullVsNaNNotEquivalent verifies that null ≢ NaN → two groups.
//
//	UNWIND [null, NaN] AS x RETURN DISTINCT x  → 2 rows
func TestDistinct_NullVsNaNNotEquivalent(t *testing.T) {
	got := collectDistinct(t, []expr.Value{expr.Null, nanFloat()})
	if got != 2 {
		t.Errorf("DISTINCT [null, NaN]: got %d rows, want 2", got)
	}
}

// TestDistinct_MixedNaNAndFinite verifies NaN ≡ NaN but NaN ≢ 1.0.
//
//	[NaN, 1.0, NaN, 2.0] → 3 distinct: NaN, 1.0, 2.0
func TestDistinct_MixedNaNAndFinite(t *testing.T) {
	got := collectDistinct(t, []expr.Value{
		nanFloat(),
		expr.FloatValue(1.0),
		nanFloat(),
		expr.FloatValue(2.0),
	})
	if got != 3 {
		t.Errorf("DISTINCT [NaN, 1.0, NaN, 2.0]: got %d rows, want 3", got)
	}
}

// TestDistinct_NaNInsideList verifies [NaN] ≡ [NaN] via recursive equivalence.
func TestDistinct_NaNInsideList(t *testing.T) {
	listA := expr.ListValue{nanFloat()}
	listB := expr.ListValue{nanFloat()}
	got := collectDistinct(t, []expr.Value{listA, listB})
	if got != 1 {
		t.Errorf("DISTINCT [[NaN],[NaN]]: got %d rows, want 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// count(DISTINCT x) tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCountDistinct_NaNEquivalence verifies count(DISTINCT x) over [NaN, NaN]
// returns 1 (CIP2016-06-14 §448).
func TestCountDistinct_NaNEquivalence(t *testing.T) {
	got := countViaDist(t, []expr.Value{nanFloat(), nanFloat()})
	if got != 1 {
		t.Errorf("count(DISTINCT [NaN, NaN]): got %d, want 1", got)
	}
}

// TestCountDistinct_NestedNull verifies count(DISTINCT x) over [[1,null],[1,null]]
// returns 1.
func TestCountDistinct_NestedNull(t *testing.T) {
	list := expr.ListValue{expr.IntegerValue(1), expr.Null}
	got := countViaDist(t, []expr.Value{list, list})
	if got != 1 {
		t.Errorf("count(DISTINCT [[1,null],[1,null]]): got %d, want 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EagerAggregation grouping tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGrouping_NaNEquivalence exercises EagerAggregation grouping directly:
// two rows keyed on NaN must land in the same group.
func TestGrouping_NaNEquivalence(t *testing.T) {
	rows := []exec.Row{
		{nanFloat()},
		{nanFloat()},
	}
	got := groupCount(t, rows)
	if got != 1 {
		t.Errorf("grouping by NaN: got %d groups, want 1", got)
	}
}

// TestGrouping_NestedNullEquivalence verifies that [1,null] used as a grouping
// key groups both rows together.
func TestGrouping_NestedNullEquivalence(t *testing.T) {
	key := expr.ListValue{expr.IntegerValue(1), expr.Null}
	rows := []exec.Row{
		{key},
		{key},
	}
	got := groupCount(t, rows)
	if got != 1 {
		t.Errorf("grouping by [1,null]: got %d groups, want 1", got)
	}
}

// TestGrouping_NullKeyEquivalence verifies null grouping key regression (must
// still produce one group for two null-keyed rows).
func TestGrouping_NullKeyEquivalence(t *testing.T) {
	rows := []exec.Row{
		{expr.Null},
		{expr.Null},
	}
	got := groupCount(t, rows)
	if got != 1 {
		t.Errorf("grouping by null: got %d groups, want 1", got)
	}
}

// TestGrouping_NaNVsFiniteDistinct verifies that NaN and 1.0 form separate groups.
func TestGrouping_NaNVsFiniteDistinct(t *testing.T) {
	rows := []exec.Row{
		{nanFloat()},
		{expr.FloatValue(1.0)},
	}
	got := groupCount(t, rows)
	if got != 2 {
		t.Errorf("grouping NaN vs 1.0: got %d groups, want 2", got)
	}
}
