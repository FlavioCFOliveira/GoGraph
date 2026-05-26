package funcs

// aggregators.go — stateful aggregator implementations for the Cypher executor.
//
// # Interface
//
// [Aggregator] is the accumulation contract. An [AggregatorFactory] is a zero-
// argument function that constructs a fresh Aggregator; it is used by
// EagerAggregation to create one instance per group.
//
// # Supported aggregators
//
//   - [CountAgg] / [CountStarAgg] — row count, NULL-skipping unless count(*)
//   - [SumAgg] — numeric sum, NULL-skipping
//   - [AvgAgg] — arithmetic mean, NULL-skipping
//   - [MinAgg] — minimum value, NULL-skipping
//   - [MaxAgg] — maximum value, NULL-skipping
//   - [CollectAgg] — collect non-NULL values into a list
//   - [StdDevAgg] — sample standard deviation (Welford online, NULL-skip)
//   - [StdDevPAgg] — population standard deviation
//   - [PercentileContAgg] — percentile by linear interpolation
//   - [PercentileDiscAgg] — percentile by nearest discrete value
//
// # NULL handling
//
// All aggregators skip NULL inputs except [CountStarAgg] which counts every row
// regardless. When no non-NULL values have been accumulated, the result is NULL
// (except count(*) / count(x) which return 0).
//
// # Concurrency
//
// Aggregator instances are NOT safe for concurrent use. Each pipeline goroutine
// owns its own set of instances, one per group per column.

import (
	"math"
	"sort"

	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Aggregator interface
// ─────────────────────────────────────────────────────────────────────────────

// Aggregator accumulates values from a single aggregate column across multiple
// input rows within one group.
//
// Lifecycle: Init → Step (once per row) → Result (once per group).
//
// Aggregator is NOT safe for concurrent use.
type Aggregator interface {
	// Init resets the aggregator to its initial (empty) state. Called once per
	// group before any Step calls for that group.
	Init()

	// Step incorporates a single input value into the running accumulation.
	// NULL handling is defined per aggregator (most skip NULLs).
	Step(v expr.Value)

	// Result returns the final aggregated value for the group. It must be called
	// exactly once after all Step calls for the group, and before any reuse.
	Result() expr.Value
}

// AggregatorFactory is a zero-argument constructor that returns a new,
// initialised Aggregator. EagerAggregation holds one factory per aggregate
// expression and calls it once per group.
type AggregatorFactory func() Aggregator

// ─────────────────────────────────────────────────────────────────────────────
// CountAgg — count(expr), NULL-skipping
// ─────────────────────────────────────────────────────────────────────────────

// CountAgg counts non-NULL input values. Corresponds to count(expr).
//
// CountAgg is NOT safe for concurrent use.
type CountAgg struct {
	n int64
}

// NewCountAgg returns an AggregatorFactory for CountAgg.
func NewCountAgg() AggregatorFactory {
	return func() Aggregator { a := &CountAgg{}; a.Init(); return a }
}

// Init resets the counter to zero.
func (a *CountAgg) Init() { a.n = 0 }

// Step increments the counter for non-NULL values.
func (a *CountAgg) Step(v expr.Value) {
	if !expr.IsNull(v) {
		a.n++
	}
}

// Result returns the count as an IntegerValue.
func (a *CountAgg) Result() expr.Value { return expr.IntegerValue(a.n) }

// ─────────────────────────────────────────────────────────────────────────────
// CountStarAgg — count(*), counts every row including NULLs
// ─────────────────────────────────────────────────────────────────────────────

// CountStarAgg counts every input row regardless of NULLs. Corresponds to
// count(*).
//
// CountStarAgg is NOT safe for concurrent use.
type CountStarAgg struct {
	n int64
}

// NewCountStarAgg returns an AggregatorFactory for CountStarAgg.
func NewCountStarAgg() AggregatorFactory {
	return func() Aggregator { a := &CountStarAgg{}; a.Init(); return a }
}

// Init resets the counter to zero.
func (a *CountStarAgg) Init() { a.n = 0 }

// Step increments the counter unconditionally.
func (a *CountStarAgg) Step(_ expr.Value) { a.n++ }

// Result returns the count as an IntegerValue.
func (a *CountStarAgg) Result() expr.Value { return expr.IntegerValue(a.n) }

// ─────────────────────────────────────────────────────────────────────────────
// SumAgg — sum(expr), NULL-skipping, promotes Int+Float → Float
// ─────────────────────────────────────────────────────────────────────────────

// SumAgg accumulates a numeric sum. Integer-only inputs produce an integer
// result; any float input causes the result to be a float.
//
// SumAgg is NOT safe for concurrent use.
type SumAgg struct {
	iSum   int64
	fSum   float64
	isF    bool
	hasAny bool
}

// NewSumAgg returns an AggregatorFactory for SumAgg.
func NewSumAgg() AggregatorFactory {
	return func() Aggregator { a := &SumAgg{}; a.Init(); return a }
}

// Init resets the accumulator.
func (a *SumAgg) Init() {
	a.iSum = 0
	a.fSum = 0
	a.isF = false
	a.hasAny = false
}

// Step adds v to the running sum, skipping NULLs. Promotes to float on the
// first FloatValue encountered.
func (a *SumAgg) Step(v expr.Value) {
	switch val := v.(type) {
	case expr.IntegerValue:
		a.hasAny = true
		if a.isF {
			a.fSum += float64(int64(val))
		} else {
			a.iSum += int64(val)
		}
	case expr.FloatValue:
		a.hasAny = true
		if !a.isF {
			// Promote: carry over integer sum.
			a.fSum = float64(a.iSum) + float64(val)
			a.isF = true
		} else {
			a.fSum += float64(val)
		}
	}
	// NULLs and non-numeric values are silently skipped.
}

// Result returns NULL if no non-NULL values were accumulated, otherwise the
// sum as IntegerValue or FloatValue.
func (a *SumAgg) Result() expr.Value {
	if !a.hasAny {
		return expr.Null
	}
	if a.isF {
		return expr.FloatValue(a.fSum)
	}
	return expr.IntegerValue(a.iSum)
}

// ─────────────────────────────────────────────────────────────────────────────
// AvgAgg — avg(expr), arithmetic mean, NULL-skipping
// ─────────────────────────────────────────────────────────────────────────────

// AvgAgg computes the arithmetic mean of numeric inputs, skipping NULLs.
//
// AvgAgg is NOT safe for concurrent use.
type AvgAgg struct {
	sum float64
	n   int64
}

// NewAvgAgg returns an AggregatorFactory for AvgAgg.
func NewAvgAgg() AggregatorFactory {
	return func() Aggregator { a := &AvgAgg{}; a.Init(); return a }
}

// Init resets the accumulator.
func (a *AvgAgg) Init() { a.sum = 0; a.n = 0 }

// Step adds v to the running total, skipping NULLs and non-numeric values.
func (a *AvgAgg) Step(v expr.Value) {
	switch val := v.(type) {
	case expr.IntegerValue:
		a.sum += float64(int64(val))
		a.n++
	case expr.FloatValue:
		a.sum += float64(val)
		a.n++
	}
}

// Result returns NULL if no non-NULL values were accumulated, otherwise a
// FloatValue representing the mean.
func (a *AvgAgg) Result() expr.Value {
	if a.n == 0 {
		return expr.Null
	}
	return expr.FloatValue(a.sum / float64(a.n))
}

// ─────────────────────────────────────────────────────────────────────────────
// MinAgg — min(expr), NULL-skipping, openCypher total ordering
// ─────────────────────────────────────────────────────────────────────────────

// MinAgg accumulates the minimum value across all non-NULL inputs using the
// openCypher total ordering defined by [expr.Compare].
//
// MinAgg is NOT safe for concurrent use.
type MinAgg struct {
	min expr.Value
}

// NewMinAgg returns an AggregatorFactory for MinAgg.
func NewMinAgg() AggregatorFactory {
	return func() Aggregator { a := &MinAgg{}; a.Init(); return a }
}

// Init resets the accumulator to "no value seen".
func (a *MinAgg) Init() { a.min = nil }

// Step updates the minimum if v is non-NULL and less than the current minimum.
func (a *MinAgg) Step(v expr.Value) {
	if expr.IsNull(v) {
		return
	}
	if a.min == nil || expr.Compare(v, a.min) < 0 {
		a.min = v
	}
}

// Result returns NULL if no non-NULL values were accumulated, otherwise the
// minimum value.
func (a *MinAgg) Result() expr.Value {
	if a.min == nil {
		return expr.Null
	}
	return a.min
}

// ─────────────────────────────────────────────────────────────────────────────
// MaxAgg — max(expr), NULL-skipping, openCypher total ordering
// ─────────────────────────────────────────────────────────────────────────────

// MaxAgg accumulates the maximum value across all non-NULL inputs using the
// openCypher total ordering defined by [expr.Compare].
//
// MaxAgg is NOT safe for concurrent use.
type MaxAgg struct {
	max expr.Value
}

// NewMaxAgg returns an AggregatorFactory for MaxAgg.
func NewMaxAgg() AggregatorFactory {
	return func() Aggregator { a := &MaxAgg{}; a.Init(); return a }
}

// Init resets the accumulator to "no value seen".
func (a *MaxAgg) Init() { a.max = nil }

// Step updates the maximum if v is non-NULL and greater than the current maximum.
func (a *MaxAgg) Step(v expr.Value) {
	if expr.IsNull(v) {
		return
	}
	if a.max == nil || expr.Compare(v, a.max) > 0 {
		a.max = v
	}
}

// Result returns NULL if no non-NULL values were accumulated, otherwise the
// maximum value.
func (a *MaxAgg) Result() expr.Value {
	if a.max == nil {
		return expr.Null
	}
	return a.max
}

// ─────────────────────────────────────────────────────────────────────────────
// CollectAgg — collect(expr), builds a ListValue, NULL-skipping
// ─────────────────────────────────────────────────────────────────────────────

// CollectAgg collects non-NULL values into an ordered [expr.ListValue].
//
// CollectAgg is NOT safe for concurrent use.
type CollectAgg struct {
	items expr.ListValue
}

// NewCollectAgg returns an AggregatorFactory for CollectAgg.
func NewCollectAgg() AggregatorFactory {
	return func() Aggregator { a := &CollectAgg{}; a.Init(); return a }
}

// Init resets the collection.
func (a *CollectAgg) Init() { a.items = nil }

// Step appends v to the collection if it is non-NULL.
func (a *CollectAgg) Step(v expr.Value) {
	if !expr.IsNull(v) {
		a.items = append(a.items, v)
	}
}

// Result returns a [expr.ListValue] containing all accumulated non-NULL values,
// or an empty list if none were collected.
func (a *CollectAgg) Result() expr.Value {
	if a.items == nil {
		return expr.ListValue{}
	}
	return a.items
}

// ─────────────────────────────────────────────────────────────────────────────
// StdDevAgg — stdev(expr), sample standard deviation (Welford online)
// ─────────────────────────────────────────────────────────────────────────────

// StdDevAgg computes the sample standard deviation using Welford's numerically
// stable online algorithm. NULLs and non-numeric values are skipped.
//
// Returns NULL if fewer than 2 non-NULL values were accumulated (sample std dev
// of a single value is undefined). Returns 0.0 for exactly 2 identical values.
//
// StdDevAgg is NOT safe for concurrent use.
type StdDevAgg struct {
	n    int64
	mean float64
	m2   float64 // Welford's M2 accumulator
}

// NewStdDevAgg returns an AggregatorFactory for StdDevAgg.
func NewStdDevAgg() AggregatorFactory {
	return func() Aggregator { a := &StdDevAgg{}; a.Init(); return a }
}

// Init resets the accumulator.
func (a *StdDevAgg) Init() { a.n = 0; a.mean = 0; a.m2 = 0 }

// Step incorporates v via Welford's online update, skipping NULLs.
func (a *StdDevAgg) Step(v expr.Value) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.n++
	delta := f - a.mean
	a.mean += delta / float64(a.n)
	a.m2 += delta * (f - a.mean)
}

// Result returns NULL if n < 2, otherwise FloatValue(sqrt(M2/(n-1))).
func (a *StdDevAgg) Result() expr.Value {
	if a.n < 2 {
		return expr.Null
	}
	return expr.FloatValue(math.Sqrt(a.m2 / float64(a.n-1)))
}

// ─────────────────────────────────────────────────────────────────────────────
// StdDevPAgg — stdevp(expr), population standard deviation
// ─────────────────────────────────────────────────────────────────────────────

// StdDevPAgg computes the population standard deviation using Welford's
// numerically stable online algorithm. NULLs and non-numeric values are
// skipped. Returns NULL if no non-NULL values were accumulated.
//
// StdDevPAgg is NOT safe for concurrent use.
type StdDevPAgg struct {
	n    int64
	mean float64
	m2   float64
}

// NewStdDevPAgg returns an AggregatorFactory for StdDevPAgg.
func NewStdDevPAgg() AggregatorFactory {
	return func() Aggregator { a := &StdDevPAgg{}; a.Init(); return a }
}

// Init resets the accumulator.
func (a *StdDevPAgg) Init() { a.n = 0; a.mean = 0; a.m2 = 0 }

// Step incorporates v via Welford's online update, skipping NULLs.
func (a *StdDevPAgg) Step(v expr.Value) {
	f, ok := toFloat64(v)
	if !ok {
		return
	}
	a.n++
	delta := f - a.mean
	a.mean += delta / float64(a.n)
	a.m2 += delta * (f - a.mean)
}

// Result returns NULL if n == 0, otherwise FloatValue(sqrt(M2/n)).
func (a *StdDevPAgg) Result() expr.Value {
	if a.n == 0 {
		return expr.Null
	}
	return expr.FloatValue(math.Sqrt(a.m2 / float64(a.n)))
}

// ─────────────────────────────────────────────────────────────────────────────
// PercentileContAgg — percentileCont(expr, p), linear interpolation
// ─────────────────────────────────────────────────────────────────────────────

// PercentileContAgg computes a percentile by linear interpolation (ANSI SQL
// PERCENTILE_CONT semantics). p must be in [0.0, 1.0]; it is clamped to that
// range. NULLs and non-numeric values are skipped.
//
// Returns NULL if no non-NULL values were accumulated.
//
// PercentileContAgg is NOT safe for concurrent use.
type PercentileContAgg struct {
	p      float64
	values []float64
}

// NewPercentileContAgg returns an AggregatorFactory for PercentileContAgg with
// the given percentile p (in [0.0, 1.0]).
func NewPercentileContAgg(p float64) AggregatorFactory {
	return func() Aggregator { a := &PercentileContAgg{p: p}; a.Init(); return a }
}

// Init resets the accumulated values.
func (a *PercentileContAgg) Init() { a.values = a.values[:0] }

// Step appends v to the list, skipping NULLs and non-numerics.
func (a *PercentileContAgg) Step(v expr.Value) {
	if f, ok := toFloat64(v); ok {
		a.values = append(a.values, f)
	}
}

// Result sorts the values and applies linear interpolation. Returns NULL if
// the list is empty.
func (a *PercentileContAgg) Result() expr.Value {
	if len(a.values) == 0 {
		return expr.Null
	}
	sort.Float64s(a.values)
	p := clamp01(a.p)
	n := len(a.values)
	if n == 1 {
		return expr.FloatValue(a.values[0])
	}
	// Linear interpolation: pos in [0, n-1].
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return expr.FloatValue(a.values[lo])
	}
	frac := pos - float64(lo)
	result := a.values[lo]*(1-frac) + a.values[hi]*frac
	return expr.FloatValue(result)
}

// ─────────────────────────────────────────────────────────────────────────────
// PercentileDiscAgg — percentileDisc(expr, p), nearest discrete value
// ─────────────────────────────────────────────────────────────────────────────

// PercentileDiscAgg computes a percentile by choosing the nearest discrete
// value in the sorted set (ANSI SQL PERCENTILE_DISC semantics). p must be in
// [0.0, 1.0]; it is clamped to that range. NULLs and non-numeric values are
// skipped.
//
// Returns NULL if no non-NULL values were accumulated.
//
// PercentileDiscAgg is NOT safe for concurrent use.
type PercentileDiscAgg struct {
	p         float64
	values    []float64
	allInt    bool
	hasValues bool
}

// NewPercentileDiscAgg returns an AggregatorFactory for PercentileDiscAgg with
// the given percentile p (in [0.0, 1.0]).
func NewPercentileDiscAgg(p float64) AggregatorFactory {
	return func() Aggregator { a := &PercentileDiscAgg{p: p}; a.Init(); return a }
}

// Init resets the accumulated values.
func (a *PercentileDiscAgg) Init() {
	a.values = a.values[:0]
	a.allInt = true
	a.hasValues = false
}

// Step appends v to the list, skipping NULLs and non-numerics.
func (a *PercentileDiscAgg) Step(v expr.Value) {
	switch n := v.(type) {
	case expr.IntegerValue:
		a.values = append(a.values, float64(int64(n)))
		a.hasValues = true
	case expr.FloatValue:
		a.values = append(a.values, float64(n))
		a.allInt = false
		a.hasValues = true
	}
}

// Result sorts the values and picks the element at the floor of p*(n-1).
// Returns NULL if the list is empty. When every input was IntegerValue,
// the result is also IntegerValue (PERCENTILE_DISC preserves the
// representation of the chosen element).
func (a *PercentileDiscAgg) Result() expr.Value {
	if !a.hasValues {
		return expr.Null
	}
	sort.Float64s(a.values)
	p := clamp01(a.p)
	n := len(a.values)
	// Index: ceil(p * n) - 1, clamped to [0, n-1].
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	v := a.values[idx]
	if a.allInt {
		return expr.IntegerValue(int64(v))
	}
	return expr.FloatValue(v)
}

// ─────────────────────────────────────────────────────────────────────────────
// Private helpers
// ─────────────────────────────────────────────────────────────────────────────

// toFloat64 extracts a float64 from an IntegerValue or FloatValue. Returns
// (0, false) for NULLs and all other types.
func toFloat64(v expr.Value) (float64, bool) {
	switch val := v.(type) {
	case expr.IntegerValue:
		return float64(int64(val)), true
	case expr.FloatValue:
		return float64(val), true
	}
	return 0, false
}

// clamp01 clamps p to [0.0, 1.0].
func clamp01(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}
