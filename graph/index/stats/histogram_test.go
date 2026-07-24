package stats

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// intCmp is a plain total order over ints, standing in for the openCypher
// orderability comparator the production instantiation supplies.
func intCmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// distinctFreqs turns a raw sample into the sorted-distinct-value + parallel-
// frequency form BuildEquiDepth consumes.
func distinctFreqs(sample []int) (vals []int, freqs []int64) {
	m := map[int]int64{}
	for _, v := range sample {
		m[v]++
	}
	for v := range m {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	for _, v := range vals {
		freqs = append(freqs, m[v])
	}
	return vals, freqs
}

// bruteSelectivity computes the exact fraction of the sample satisfying the
// predicate, the ground truth the histogram estimate is checked against.
func bruteSelectivity(sample []int, bound int, op Op) float64 {
	var c int
	for _, v := range sample {
		var ok bool
		switch op {
		case OpLt:
			ok = v < bound
		case OpLe:
			ok = v <= bound
		case OpGt:
			ok = v > bound
		case OpGe:
			ok = v >= bound
		}
		if ok {
			c++
		}
	}
	return float64(c) / float64(len(sample))
}

const histB = 256

// TestHistogram_SelectivityErrorSkewed asserts the certified 1/B bound holds on
// heavily skewed columns for every operator and a wide range of bounds — the core
// distribution-free guarantee of the equi-depth histogram with MCV-spike
// isolation.
func TestHistogram_SelectivityErrorSkewed(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5747))
	const n = 50000

	build := func(sample []int, heavy map[int]bool) *Histogram[int] {
		vals, freqs := distinctFreqs(sample)
		isHeavy := func(v int) bool { return heavy[v] }
		return BuildEquiDepth(vals, freqs, histB, isHeavy)
	}

	// A Zipf-like skew: a few values dominate, a long tail of rare values.
	skewed := make([]int, 0, n)
	zipf := rand.NewZipf(rng, 1.3, 1, 2000)
	for i := 0; i < n; i++ {
		skewed = append(skewed, int(zipf.Uint64()))
	}
	// Add a hard spike far more frequent than any bucket target.
	for i := 0; i < n/5; i++ {
		skewed = append(skewed, 424242)
	}

	h := build(skewed, map[int]bool{424242: true, 0: true, 1: true})
	tol := 1.0/float64(histB) + 1e-9

	ops := []Op{OpLt, OpLe, OpGt, OpGe}
	for _, bound := range []int{-5, 0, 1, 2, 5, 50, 500, 1999, 2000, 424242, 999999} {
		for _, op := range ops {
			est := h.Selectivity(bound, op, intCmp)
			truth := bruteSelectivity(skewed, bound, op)
			if math.Abs(est-truth) > tol {
				t.Errorf("bound=%d op=%d: est=%.5f truth=%.5f |err|=%.5f > 1/B=%.5f",
					bound, op, est, truth, math.Abs(est-truth), 1.0/float64(histB))
			}
		}
	}
}

// TestHistogram_SelectivityErrorUniform checks the bound on a uniform column too,
// where equi-depth degenerates to near-equi-width.
func TestHistogram_SelectivityErrorUniform(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	const n = 40000
	sample := make([]int, n)
	for i := range sample {
		sample[i] = rng.Intn(10000)
	}
	vals, freqs := distinctFreqs(sample)
	h := BuildEquiDepth(vals, freqs, histB, nil)
	tol := 1.0/float64(histB) + 1e-9
	for _, op := range []Op{OpLt, OpLe, OpGt, OpGe} {
		for _, bound := range []int{-1, 0, 137, 5000, 9999, 10000, 20000} {
			est := h.Selectivity(bound, op, intCmp)
			truth := bruteSelectivity(sample, bound, op)
			if math.Abs(est-truth) > tol {
				t.Errorf("uniform bound=%d op=%d est=%.5f truth=%.5f err=%.5f",
					bound, op, est, truth, math.Abs(est-truth))
			}
		}
	}
}

// TestHistogram_HeavyValueExact confirms MCV-spike isolation makes a boundary on
// a heavy value exact (zero error), the reason singletons exist.
func TestHistogram_HeavyValueExact(t *testing.T) {
	sample := make([]int, 0, 10000)
	for i := 0; i < 3000; i++ {
		sample = append(sample, 7) // the spike
	}
	for i := 0; i < 7000; i++ {
		sample = append(sample, i%1000+100) // spread away from 7
	}
	vals, freqs := distinctFreqs(sample)
	h := BuildEquiDepth(vals, freqs, histB, func(v int) bool { return v == 7 })

	// value == 7 must resolve exactly for <, <=, >, >=.
	for _, tc := range []struct {
		op   Op
		want float64
	}{
		{OpLt, bruteSelectivity(sample, 7, OpLt)},
		{OpLe, bruteSelectivity(sample, 7, OpLe)},
		{OpGt, bruteSelectivity(sample, 7, OpGt)},
		{OpGe, bruteSelectivity(sample, 7, OpGe)},
	} {
		if got := h.Selectivity(7, tc.op, intCmp); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("op=%d on heavy value: got %.6f want %.6f (must be exact)", tc.op, got, tc.want)
		}
	}
}

func TestHistogram_Empty(t *testing.T) {
	h := BuildEquiDepth[int](nil, nil, histB, nil)
	if h.Total() != 0 {
		t.Errorf("empty total = %d, want 0", h.Total())
	}
	if got := h.Selectivity(10, OpLt, intCmp); got != 0 {
		t.Errorf("empty selectivity = %v, want 0", got)
	}
}

func TestHistogram_MonotoneCumulative(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	sample := make([]int, 20000)
	for i := range sample {
		sample[i] = rng.Intn(5000)
	}
	vals, freqs := distinctFreqs(sample)
	h := BuildEquiDepth(vals, freqs, histB, nil)
	// Cumulative counts must be non-decreasing and end at total; no bucket may
	// exceed the equi-depth target (heavy singletons excepted).
	var prev int64
	for i := range h.buckets {
		if h.buckets[i].Cum < prev {
			t.Fatalf("bucket %d cum %d < prev %d", i, h.buckets[i].Cum, prev)
		}
		prev = h.buckets[i].Cum
	}
	if prev != h.total {
		t.Errorf("last cum %d != total %d", prev, h.total)
	}
}
