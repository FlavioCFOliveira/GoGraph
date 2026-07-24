package exec

// agg_column_kernel.go — columnar (SoA) aggregate accumulators for the chunk-input
// [EagerAggregation] path (rmp #2104, columnar value-model epic #1704).
//
// # What this replaces
//
// On the row-input path each group holds one boxed [funcs.Aggregator] per aggregate
// (an array-of-structs of interface values) and [funcs.Aggregator.Step] is fed a
// boxed [expr.Value] per row. Over a columnar input that is an O(input) waste: the
// argument already lives unboxed in a typed [Chunk] column, yet the old chunk path
// re-boxed every cell via [Chunk.BoxCell] just to satisfy Step.
//
// An [aggKernel] instead keeps its accumulation in group-id-indexed PARALLEL ARRAYS
// (struct-of-arrays) and scatter-accumulates the unboxed argument column directly:
// `sum[gid[row]] += arg[row]` (MonetDB/X100 hash-aggregation, Boncz et al. CIDR 2005;
// DuckDB UpdateStates). Boxing survives only at the O(groups) result boundary.
//
// # Equivalence and reversibility (design §6)
//
// Every kernel is equivalent to the [funcs.Aggregator] it replaces BY CONSTRUCTION:
//
//   - The streaming kernels (count/sum/avg/min/max) mirror their aggregator's exact
//     accumulation. SUM keeps the exact int64 accumulator with the identical overflow
//     contract, promoting to a float64 accumulator on the first float exactly as
//     [funcs.SumAgg] does, so an integer SUM is never lost to float64 rounding.
//   - For a column storage the kernel cannot take unboxed (a promoted/heterogeneous
//     boxed column, a string/bool column under a numeric aggregate, a non-numeric
//     cell under min/max) it boxes THAT cell via [Chunk.BoxCell] and funnels it into
//     the SAME state transition — so a heterogeneous batch, or a numeric column that
//     later turns non-numeric, stays byte-identical to the row path.
//   - Group-key equivalence is untouched: hashing/collision resolution stay in
//     [EagerAggregation] over [expr.EquivalentHash]/[expr.Equivalent] (#2049/#2050).
//
// # Fallback
//
// Buffering aggregators (collect, percentileCont/Disc), the standard deviations, and
// any DISTINCT-wrapped aggregate have no vectorized form; [buildAggKernels] routes
// them to a [boxedKernel] that keeps one boxed [funcs.Aggregator] per group and Steps
// the boxed cell — identical to the row path, with the #1841 per-aggregator element
// budget enforced unchanged inside the aggregator.
//
// # Concurrency
//
// An aggKernel is NOT safe for concurrent use; it is owned by a single
// [EagerAggregation], which is itself single-consumer.

import (
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// aggKernel accumulates one aggregate slot over the chunk-input consume path, keyed
// by dense group id. grow is called whenever the group count rises so the SoA/AoS
// arrays stay addressable by every live gid; stepColumn scatter-accumulates one
// argument column for a batch (row r of the batch belongs to group gids[r]); result
// boxes the final value for one group.
type aggKernel interface {
	// grow ensures the kernel can address group ids [0, ngroups).
	grow(ngroups int)
	// stepColumn scatter-accumulates argument column argCol of src for rows
	// [0, len(gids)), each into group gids[row]. An argCol past src's column count
	// is treated as an all-NULL column (matching the row path's out-of-range cell).
	stepColumn(src *Chunk, argCol int, gids []int) error
	// result returns the boxed aggregate value for group gid.
	result(gid int) expr.Value
}

// buildAggKernels chooses one kernel per aggregate slot by probing the factory's
// concrete aggregator type. The streaming aggregators (count(*)/count/sum/avg/min/max)
// get a vectorized SoA kernel; every other aggregator — the buffering ones and any
// DISTINCT-wrapped aggregate, whose factory returns a wrapper type — falls back to a
// [boxedKernel]. The probe instance is discarded for the vectorized kernels and
// re-created per group by [boxedKernel] via the same factory, so no state leaks.
func buildAggKernels(factories []funcs.AggregatorFactory) []aggKernel {
	kernels := make([]aggKernel, len(factories))
	for i, f := range factories {
		switch f().(type) {
		case *funcs.CountStarAgg:
			kernels[i] = &countStarKernel{}
		case *funcs.CountAgg:
			kernels[i] = &countKernel{}
		case *funcs.SumAgg:
			kernels[i] = &sumKernel{}
		case *funcs.AvgAgg:
			kernels[i] = &avgKernel{}
		case *funcs.MinAgg:
			kernels[i] = &minMaxKernel{isMax: false}
		case *funcs.MaxAgg:
			kernels[i] = &minMaxKernel{isMax: true}
		default:
			kernels[i] = &boxedKernel{factory: f}
		}
	}
	return kernels
}

// ─────────────────────────────────────────────────────────────────────────────
// count(*) — counts every row (mirrors funcs.CountStarAgg)
// ─────────────────────────────────────────────────────────────────────────────

type countStarKernel struct{ n []int64 }

func (k *countStarKernel) grow(ngroups int) { k.n = growInt64(k.n, ngroups) }

func (k *countStarKernel) stepColumn(_ *Chunk, _ int, gids []int) error {
	for _, gid := range gids {
		k.n[gid]++
	}
	return nil
}

func (k *countStarKernel) result(gid int) expr.Value { return expr.IntegerValue(k.n[gid]) }

// ─────────────────────────────────────────────────────────────────────────────
// count(expr) — counts non-NULL rows (mirrors funcs.CountAgg)
// ─────────────────────────────────────────────────────────────────────────────

type countKernel struct{ n []int64 }

func (k *countKernel) grow(ngroups int) { k.n = growInt64(k.n, ngroups) }

func (k *countKernel) stepColumn(src *Chunk, argCol int, gids []int) error {
	if argCol >= src.NumCols() {
		return nil // out-of-range column reads as all-NULL: nothing to count
	}
	// The validity bitmap is authoritative for every column kind (including boxed),
	// so this is correct without inspecting the backing type: a cell is counted iff
	// it boxes to a non-NULL value, exactly as funcs.CountAgg's !IsNull check.
	for row, gid := range gids {
		if src.IsValid(argCol, row) {
			k.n[gid]++
		}
	}
	return nil
}

func (k *countKernel) result(gid int) expr.Value { return expr.IntegerValue(k.n[gid]) }

// ─────────────────────────────────────────────────────────────────────────────
// sum(expr) — exact int64 with float promotion (mirrors funcs.SumAgg)
// ─────────────────────────────────────────────────────────────────────────────

// sumKernel mirrors [funcs.SumAgg] per group: an exact int64 accumulator until the
// first float promotes the group to a float64 accumulator. An integer SUM is never
// routed through float64, so large-integer sums stay exact and bit-identical to the
// row path.
type sumKernel struct {
	iSum   []int64
	fSum   []float64
	isF    []bool
	hasAny []bool
}

func (k *sumKernel) grow(ngroups int) {
	k.iSum = growInt64(k.iSum, ngroups)
	k.fSum = growFloat64(k.fSum, ngroups)
	k.isF = growBool(k.isF, ngroups)
	k.hasAny = growBool(k.hasAny, ngroups)
}

// stepInt folds an integer value into group gid, replicating funcs.SumAgg's '+' guard
// (identical overflow message) so an integer SUM that overflows int64 fails exactly
// as the row path does — same running sum, same operand.
func (k *sumKernel) stepInt(gid int, iv int64) error {
	k.hasAny[gid] = true
	if k.isF[gid] {
		k.fSum[gid] += float64(iv)
		return nil
	}
	s := k.iSum[gid]
	if iv > 0 && s > math.MaxInt64-iv {
		return &expr.EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: sum() result exceeds Int64 range (positive overflow at %d + %d)", s, iv)}
	}
	if iv < 0 && s < math.MinInt64-iv {
		return &expr.EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: sum() result exceeds Int64 range (negative overflow at %d + %d)", s, iv)}
	}
	k.iSum[gid] = s + iv
	return nil
}

// stepFloat folds a float value into group gid, promoting the accumulator from int to
// float on first sight exactly as funcs.SumAgg does (carrying the exact integer sum
// over once, then accumulating in float64).
func (k *sumKernel) stepFloat(gid int, fv float64) {
	k.hasAny[gid] = true
	if !k.isF[gid] {
		k.fSum[gid] = float64(k.iSum[gid]) + fv
		k.isF[gid] = true
	} else {
		k.fSum[gid] += fv
	}
}

func (k *sumKernel) stepColumn(src *Chunk, argCol int, gids []int) error {
	if argCol >= src.NumCols() {
		return nil
	}
	switch {
	case src.IsInt64Column(argCol):
		data, valid, allValid := src.Int64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				if err := k.stepInt(gid, data[row]); err != nil {
					return err
				}
			}
		}
	case src.IsFloat64Column(argCol):
		data, valid, allValid := src.Float64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				k.stepFloat(gid, data[row])
			}
		}
	default:
		// Boxed / promoted / string / bool column: mirror funcs.SumAgg.Step on the
		// boxed cell — integers and floats fold, NULL and non-numeric skip.
		for row, gid := range gids {
			switch val := src.BoxCell(argCol, row).(type) {
			case expr.IntegerValue:
				if err := k.stepInt(gid, int64(val)); err != nil {
					return err
				}
			case expr.FloatValue:
				k.stepFloat(gid, float64(val))
			}
		}
	}
	return nil
}

// result mirrors funcs.SumAgg.Result: an empty/all-NULL group yields integer 0 (NOT
// NULL), a promoted group a float, and an integer-only group the exact int64 sum.
func (k *sumKernel) result(gid int) expr.Value {
	if !k.hasAny[gid] {
		return expr.IntegerValue(0)
	}
	if k.isF[gid] {
		return expr.FloatValue(k.fSum[gid])
	}
	return expr.IntegerValue(k.iSum[gid])
}

// ─────────────────────────────────────────────────────────────────────────────
// avg(expr) — arithmetic mean (mirrors funcs.AvgAgg)
// ─────────────────────────────────────────────────────────────────────────────

// avgKernel mirrors [funcs.AvgAgg] per group: a float64 running sum and an int64
// count. AvgAgg accumulates integer inputs through the same float64 sum, so the
// kernel does too — bit-identical, with no separate exact-integer path.
type avgKernel struct {
	sum []float64
	n   []int64
}

func (k *avgKernel) grow(ngroups int) {
	k.sum = growFloat64(k.sum, ngroups)
	k.n = growInt64(k.n, ngroups)
}

func (k *avgKernel) stepColumn(src *Chunk, argCol int, gids []int) error {
	if argCol >= src.NumCols() {
		return nil
	}
	switch {
	case src.IsInt64Column(argCol):
		data, valid, allValid := src.Int64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				k.sum[gid] += float64(data[row])
				k.n[gid]++
			}
		}
	case src.IsFloat64Column(argCol):
		data, valid, allValid := src.Float64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				k.sum[gid] += data[row]
				k.n[gid]++
			}
		}
	default:
		for row, gid := range gids {
			switch val := src.BoxCell(argCol, row).(type) {
			case expr.IntegerValue:
				k.sum[gid] += float64(int64(val))
				k.n[gid]++
			case expr.FloatValue:
				k.sum[gid] += float64(val)
				k.n[gid]++
			}
		}
	}
	return nil
}

// result mirrors funcs.AvgAgg.Result: NULL when the group saw no numeric value, else
// the float64 mean.
func (k *avgKernel) result(gid int) expr.Value {
	if k.n[gid] == 0 {
		return expr.Null
	}
	return expr.FloatValue(k.sum[gid] / float64(k.n[gid]))
}

// ─────────────────────────────────────────────────────────────────────────────
// min/max(expr) — openCypher total ordering (mirrors funcs.MinAgg / funcs.MaxAgg)
// ─────────────────────────────────────────────────────────────────────────────

const (
	mmEmpty uint8 = iota // group has seen no non-NULL value
	mmInt                // best is an unboxed int64 (bi)
	mmFloat              // best is an unboxed float64 (bf)
	mmBoxed              // best is a boxed expr.Value (bv) — a non-numeric was seen
)

// minMaxKernel mirrors [funcs.MinAgg]/[funcs.MaxAgg]: it keeps the running best per
// group and updates it under the openCypher total order (NaN last). The best is
// stored UNBOXED while every value a group has seen is numeric (the common case — an
// integer or float property column); the moment a group sees a non-numeric value it
// promotes to a boxed best (seeded with the current numeric best) so cross-kind
// min/max (e.g. min over a heterogeneous property) stays exactly what MinAgg would
// compute.
//
// A homogeneous integer column compares int↔int with the exact primitive ordering
// (allocation-free — integer order carries no openCypher subtlety and equals
// [expr.Compare] on two integers). Every comparison that involves a float or a
// non-numeric operand delegates to [expr.Compare] so float/NaN and cross-type
// int↔float ordering is never reimplemented; those operands are boxed for the call,
// which for a genuinely float or heterogeneous column is the accepted cost of exact
// ordering (correctness over allocation, and still far below the row path).
type minMaxKernel struct {
	isMax bool
	mode  []uint8
	bi    []int64
	bf    []float64
	bv    []expr.Value
}

func (k *minMaxKernel) grow(ngroups int) {
	k.mode = growUint8(k.mode, ngroups)
	k.bi = growInt64(k.bi, ngroups)
	k.bf = growFloat64(k.bf, ngroups)
	k.bv = growValue(k.bv, ngroups)
}

// betterI reports whether integer candidate improves on integer best under this
// kernel's direction. Integer order is exact and unambiguous (it equals
// [expr.Compare] on two integers), so this stays allocation-free on the hot path.
func (k *minMaxKernel) betterI(candidate, best int64) bool {
	if k.isMax {
		return candidate > best
	}
	return candidate < best
}

// betterV reports whether candidate improves on best under [expr.Compare] (the
// openCypher total order, NaN last), mirroring MinAgg (Compare(v,min) < 0) / MaxAgg
// (Compare(v,max) > 0). It is used whenever a float or non-numeric operand is
// involved so float/NaN and cross-type ordering is never reimplemented.
func (k *minMaxKernel) betterV(candidate, best expr.Value) bool {
	c := expr.Compare(candidate, best)
	if k.isMax {
		return c > 0
	}
	return c < 0
}

func (k *minMaxKernel) stepColumn(src *Chunk, argCol int, gids []int) error {
	if argCol >= src.NumCols() {
		return nil
	}
	switch {
	case src.IsInt64Column(argCol):
		data, valid, allValid := src.Int64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				k.stepInt(gid, data[row])
			}
		}
	case src.IsFloat64Column(argCol):
		data, valid, allValid := src.Float64Column(argCol)
		for row, gid := range gids {
			if allValid || BitSet(valid, row) {
				k.stepFloat(gid, data[row])
			}
		}
	default:
		for row, gid := range gids {
			v := src.BoxCell(argCol, row)
			if expr.IsNull(v) {
				continue
			}
			switch cv := v.(type) {
			case expr.IntegerValue:
				k.stepInt(gid, int64(cv))
			case expr.FloatValue:
				k.stepFloat(gid, float64(cv))
			default:
				k.stepBoxed(gid, v)
			}
		}
	}
	return nil
}

// stepInt folds an integer candidate into group gid, keeping the best unboxed while
// the group stays numeric. An int↔int comparison uses the exact primitive ordering
// (allocation-free); an int↔float or int↔boxed comparison delegates to expr.Compare.
func (k *minMaxKernel) stepInt(gid int, iv int64) {
	switch k.mode[gid] {
	case mmEmpty:
		k.mode[gid] = mmInt
		k.bi[gid] = iv
	case mmInt:
		if k.betterI(iv, k.bi[gid]) {
			k.bi[gid] = iv
		}
	case mmFloat:
		if k.betterV(expr.IntegerValue(iv), expr.FloatValue(k.bf[gid])) {
			k.mode[gid] = mmInt
			k.bi[gid] = iv
		}
	default: // mmBoxed
		if k.betterV(expr.IntegerValue(iv), k.bv[gid]) {
			k.bv[gid] = expr.IntegerValue(iv)
		}
	}
}

// stepFloat folds a float candidate into group gid. Any comparison involving a float
// operand goes through expr.Compare so float/NaN and cross-type ordering is exact.
func (k *minMaxKernel) stepFloat(gid int, fv float64) {
	switch k.mode[gid] {
	case mmEmpty:
		k.mode[gid] = mmFloat
		k.bf[gid] = fv
	case mmInt:
		if k.betterV(expr.FloatValue(fv), expr.IntegerValue(k.bi[gid])) {
			k.mode[gid] = mmFloat
			k.bf[gid] = fv
		}
	case mmFloat:
		if k.betterV(expr.FloatValue(fv), expr.FloatValue(k.bf[gid])) {
			k.bf[gid] = fv
		}
	default: // mmBoxed
		if k.betterV(expr.FloatValue(fv), k.bv[gid]) {
			k.bv[gid] = expr.FloatValue(fv)
		}
	}
}

// stepBoxed folds a NON-numeric (non-NULL) candidate into group gid, promoting the
// group to a boxed best. Once boxed, all further comparisons (numeric or not) run
// through expr.Compare against the boxed best, so a heterogeneous min/max is exactly
// what funcs.MinAgg/MaxAgg would compute over the same values.
func (k *minMaxKernel) stepBoxed(gid int, v expr.Value) {
	switch k.mode[gid] {
	case mmEmpty:
		k.mode[gid] = mmBoxed
		k.bv[gid] = v
	case mmInt:
		best := expr.Value(expr.IntegerValue(k.bi[gid]))
		if k.betterV(v, best) {
			best = v
		}
		k.mode[gid] = mmBoxed
		k.bv[gid] = best
	case mmFloat:
		best := expr.Value(expr.FloatValue(k.bf[gid]))
		if k.betterV(v, best) {
			best = v
		}
		k.mode[gid] = mmBoxed
		k.bv[gid] = best
	default: // mmBoxed
		if k.betterV(v, k.bv[gid]) {
			k.bv[gid] = v
		}
	}
}

// result mirrors funcs.MinAgg/MaxAgg.Result: NULL when the group saw no non-NULL
// value, else the winning value boxed once.
func (k *minMaxKernel) result(gid int) expr.Value {
	switch k.mode[gid] {
	case mmInt:
		return expr.IntegerValue(k.bi[gid])
	case mmFloat:
		return expr.FloatValue(k.bf[gid])
	case mmBoxed:
		return k.bv[gid]
	default: // mmEmpty
		return expr.Null
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// boxed fallback — one funcs.Aggregator per group (collect, percentile, stdev,
// DISTINCT-wrapped, and any aggregator without a vectorized form)
// ─────────────────────────────────────────────────────────────────────────────

// boxedKernel keeps one boxed [funcs.Aggregator] per group and Steps the boxed cell,
// identical to the row path. The per-aggregator element budget (#1841) is enforced
// inside the aggregator (baked into the factory), so a buffering aggregator still
// fails fast with its typed error.
type boxedKernel struct {
	factory funcs.AggregatorFactory
	aggs    []funcs.Aggregator
}

func (k *boxedKernel) grow(ngroups int) {
	for len(k.aggs) < ngroups {
		k.aggs = append(k.aggs, k.factory()) // factory() returns an Init-ed aggregator
	}
}

func (k *boxedKernel) stepColumn(src *Chunk, argCol int, gids []int) error {
	ncols := src.NumCols()
	for row, gid := range gids {
		v := expr.Value(expr.Null)
		if argCol < ncols {
			v = src.BoxCell(argCol, row)
		}
		if err := k.aggs[gid].Step(v); err != nil {
			return err
		}
	}
	return nil
}

func (k *boxedKernel) result(gid int) expr.Value { return k.aggs[gid].Result() }

// ─────────────────────────────────────────────────────────────────────────────
// SoA growth helpers — extend a parallel array to ngroups, retaining backing
// ─────────────────────────────────────────────────────────────────────────────

func growInt64(s []int64, n int) []int64 {
	if n <= len(s) {
		return s
	}
	if cap(s) >= n {
		return s[:n]
	}
	grown := make([]int64, n)
	copy(grown, s)
	return grown
}

func growFloat64(s []float64, n int) []float64 {
	if n <= len(s) {
		return s
	}
	if cap(s) >= n {
		return s[:n]
	}
	grown := make([]float64, n)
	copy(grown, s)
	return grown
}

func growBool(s []bool, n int) []bool {
	if n <= len(s) {
		return s
	}
	if cap(s) >= n {
		return s[:n]
	}
	grown := make([]bool, n)
	copy(grown, s)
	return grown
}

func growUint8(s []uint8, n int) []uint8 {
	if n <= len(s) {
		return s
	}
	if cap(s) >= n {
		return s[:n]
	}
	grown := make([]uint8, n)
	copy(grown, s)
	return grown
}

func growValue(s []expr.Value, n int) []expr.Value {
	if n <= len(s) {
		return s
	}
	if cap(s) >= n {
		return s[:n]
	}
	grown := make([]expr.Value, n)
	copy(grown, s)
	return grown
}
