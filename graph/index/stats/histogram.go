package stats

// Op is a range-predicate operator the histogram can estimate the selectivity of.
type Op uint8

const (
	// OpLt is the strict less-than predicate (value < bound).
	OpLt Op = iota
	// OpLe is the less-than-or-equal predicate (value <= bound).
	OpLe
	// OpGt is the strict greater-than predicate (value > bound).
	OpGt
	// OpGe is the greater-than-or-equal predicate (value >= bound).
	OpGe
)

// bucket is one equi-depth histogram bucket. Upper is the greatest value in the
// bucket; Count is the number of rows it holds; Cum is the running total up to
// and including this bucket. Singleton marks a bucket that isolates exactly one
// most-common value (Count rows all equal to Upper), which makes an equality or
// range boundary landing on a heavy value exact rather than blurred.
type bucket[T any] struct {
	Upper     T
	Count     int64
	Cum       int64
	Singleton bool
}

// Histogram is an equi-depth histogram over an ordered value domain T. Its
// worst-case absolute selectivity error is the distribution-free 1/B bound: no
// non-singleton bucket holds more than ⌈total/B⌉ rows, and heavy values are
// isolated into singleton buckets so a boundary on a spike is exact. It is
// immutable once built ([BuildEquiDepth]) and safe for concurrent reads; the
// orderability comparator is supplied per call so the structure never captures a
// closure and stays copy-cheap.
type Histogram[T any] struct {
	buckets []bucket[T]
	total   int64 // in-domain rows summarised (the histogram's denominator)
	b       int   // the target bucket count B, the source of the 1/B error bound
}

// BuildEquiDepth builds an equi-depth histogram over the sorted distinct values
// and their parallel frequencies. values must already be sorted strictly
// ascending in the caller's orderability order (one entry per distinct value);
// freqs[i] is the row count of values[i]. The build walks that order directly and
// never re-compares, so no comparator is needed here — [Histogram.Selectivity]
// takes it at query time. b is the target bucket count B (the 1/B error bound).
// isHeavy reports whether a value must be isolated into its own singleton bucket
// (the caller passes the exact most-common-values set); any value whose own
// frequency reaches the per-bucket target is isolated as well, so the 1/B bound
// survives skew even for a spike the caller did not flag.
//
// Callers must exclude out-of-domain and NaN rows before calling (a range
// predicate yields null on NaN, so such rows belong to no bucket); their count is
// tracked separately by the caller for the estimate's denominator arithmetic.
func BuildEquiDepth[T any](values []T, freqs []int64, b int, isHeavy func(T) bool) *Histogram[T] {
	if b < 1 {
		b = 1
	}
	h := &Histogram[T]{b: b}
	var total int64
	for _, f := range freqs {
		total += f
	}
	h.total = total
	if total == 0 || len(values) == 0 {
		return h
	}

	// Per-bucket target depth q = ⌈total/B⌉: no non-singleton bucket exceeds it,
	// which is exactly the invariant the distribution-free 1/B bound rests on.
	q := (total + int64(b) - 1) / int64(b)
	if q < 1 {
		q = 1
	}

	var cum int64
	var curCount int64
	var curUpper T
	haveCur := false

	flush := func() {
		if !haveCur {
			return
		}
		cum += curCount
		h.buckets = append(h.buckets, bucket[T]{Upper: curUpper, Count: curCount, Cum: cum})
		curCount = 0
		haveCur = false
	}

	for i := range values {
		v, f := values[i], freqs[i]
		heavy := f >= q || (isHeavy != nil && isHeavy(v))
		if heavy {
			flush()
			cum += f
			h.buckets = append(h.buckets, bucket[T]{Upper: v, Count: f, Cum: cum, Singleton: true})
			continue
		}
		curCount += f
		curUpper = v
		haveCur = true
		if curCount >= q {
			flush()
		}
	}
	flush()
	return h
}

// Total reports the number of in-domain rows the histogram summarises — the
// denominator of the fraction [Histogram.Selectivity] returns.
func (h *Histogram[T]) Total() int64 { return h.total }

// Buckets reports the number of buckets, for observability and testing.
func (h *Histogram[T]) Buckets() int { return len(h.buckets) }

// BucketError returns the histogram's certified absolute selectivity error 1/B,
// the distribution-free per-boundary bound. It is independent of the data
// distribution.
func (h *Histogram[T]) BucketError() float64 {
	if h.b < 1 {
		return 1
	}
	return 1.0 / float64(h.b)
}

// Selectivity returns the estimated fraction of in-domain rows that satisfy
// value <op> bound, clamped to [0,1]. The estimate is exact for a boundary that
// lands on a singleton (heavy) value; for a boundary inside a non-singleton
// bucket it takes the bucket midpoint, so the absolute error is at most half the
// bucket depth and therefore within the 1/B bound the histogram guarantees. cmp
// is the orderability comparator over T (the Cypher layer supplies expr.Compare,
// whose cross-type Integer/Float path is the exact cmpInt64Float64).
func (h *Histogram[T]) Selectivity(bound T, op Op, cmp func(a, b T) int) float64 {
	if h.total == 0 {
		return 0
	}
	less, equal := h.split(bound, cmp)
	var rows float64
	switch op {
	case OpLt:
		rows = less
	case OpLe:
		rows = less + equal
	case OpGt:
		rows = float64(h.total) - less - equal
	case OpGe:
		rows = float64(h.total) - less
	}
	return clamp01(rows / float64(h.total))
}

// split estimates the number of rows strictly below bound and the number equal to
// bound. A singleton bucket contributes exactly (all its rows share Upper); a
// non-singleton bucket whose range straddles bound contributes its rows at the
// midpoint (half below, half not), bounding the error by half the bucket depth.
func (h *Histogram[T]) split(bound T, cmp func(a, b T) int) (less, equal float64) {
	for i := range h.buckets {
		bk := &h.buckets[i]
		c := cmp(bk.Upper, bound)
		switch {
		case c < 0:
			// Entire bucket is below bound.
			less += float64(bk.Count)
		case c == 0:
			if bk.Singleton {
				equal += float64(bk.Count) // all rows equal bound, exactly
			} else {
				// Range bucket ending exactly at bound: its rows are ≤ bound with an
				// unknown split between < and =. Midpoint.
				half := float64(bk.Count) / 2
				less += half
				equal += half
			}
			return less, equal
		default: // c > 0: bound is below this bucket's Upper.
			if bk.Singleton {
				// Singleton value strictly above bound: contributes to neither.
				return less, equal
			}
			// Non-singleton bucket whose range straddles bound (all lower buckets
			// were fully below). Midpoint of this bucket is < bound.
			less += float64(bk.Count) / 2
			return less, equal
		}
	}
	return less, equal
}

// clamp01 clamps x to the closed unit interval [0,1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
