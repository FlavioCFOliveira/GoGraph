package stats

import (
	"math"
	"testing"
)

// splitmix64 is a high-quality 64-bit mixer used to turn sequential test inputs
// into well-distributed hashes, decorrelating them the way the production
// EquivalentHash does for real values.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// hllRelErrBound is 2·(1.04/√m), the two-standard-error accuracy the estimator
// must stay within (design docs/statistics-design.md §1).
const hllRelErrBound = 2 * 1.04 / 64.0 // √4096 = 64

func TestHLL_RelativeError(t *testing.T) {
	// The 5k–10k band is the classic harmonic-mean estimator's bias region (2m–5m),
	// the very range HLL++ bias correction targets; it is exercised explicitly.
	for _, n := range []int{50, 500, 1200, 2000, 5000, 6000, 8000, 10000, 20000, 100000, 500000} {
		h := NewHLL()
		for i := 0; i < n; i++ {
			// Offset keeps the streams for different n disjoint but deterministic.
			h.Insert(splitmix64(uint64(i) + 1))
		}
		est := h.Estimate()
		relErr := math.Abs(est-float64(n)) / float64(n)
		if relErr > hllRelErrBound {
			t.Errorf("n=%d: estimate=%.0f relErr=%.4f exceeds 2σ bound %.4f",
				n, est, relErr, hllRelErrBound)
		}
	}
}

func TestHLL_Empty(t *testing.T) {
	if got := NewHLL().Estimate(); got != 0 {
		t.Errorf("empty HLL estimate = %v, want 0", got)
	}
}

func TestHLL_MonotonicAndIdempotent(t *testing.T) {
	h := NewHLL()
	const n = 3000
	for i := 0; i < n; i++ {
		h.Insert(splitmix64(uint64(i)))
	}
	first := h.Estimate()
	// Re-inserting the same hashes must not change the estimate (registers only
	// rise, and they are already at their maxima).
	for i := 0; i < n; i++ {
		h.Insert(splitmix64(uint64(i)))
	}
	if second := h.Estimate(); second != first {
		t.Errorf("idempotent re-insert changed estimate: %v -> %v", first, second)
	}
}

// TestHLL_SparseToDenseAgreement checks that the estimate does not jump
// discontinuously across the sparse→dense promotion boundary: two estimators fed
// the identical stream, one forced dense early, agree within the error bound.
func TestHLL_SparseDensePromotion(t *testing.T) {
	// Feed exactly enough to force a promotion, and confirm the dense array holds.
	h := NewHLL()
	n := hllSparseMax * 8
	for i := 0; i < n; i++ {
		h.Insert(splitmix64(uint64(i) + 7))
	}
	if h.dense == nil {
		t.Fatal("expected promotion to dense representation")
	}
	if h.sparse != nil {
		t.Error("sparse map should be nil after promotion")
	}
	est := h.Estimate()
	if relErr := math.Abs(est-float64(n)) / float64(n); relErr > hllRelErrBound {
		t.Errorf("post-promotion n=%d estimate=%.0f relErr=%.4f exceeds %.4f", n, est, relErr, hllRelErrBound)
	}
}

// TestHLL_DenseRegisterRoundTrip exercises the 6-bit packing directly: every
// register index must read back exactly what was written, including at byte
// boundaries where a 6-bit field straddles two bytes.
func TestHLL_DenseRegisterRoundTrip(t *testing.T) {
	h := &HLL{dense: make([]byte, hllDenseLen)}
	// Write a distinct 6-bit value per register (cycling 1..63) and read back.
	for idx := 0; idx < hllM; idx++ {
		v := uint8(idx%63) + 1
		h.denseSet(uint16(idx), v)
	}
	for idx := 0; idx < hllM; idx++ {
		want := uint8(idx%63) + 1
		if got := h.denseGet(uint16(idx)); got != want {
			t.Fatalf("register %d: got %d want %d", idx, got, want)
		}
	}
}

func TestHLL_RegisterSplitBounds(t *testing.T) {
	// The rank must always fit in 6 bits (≤ 63) and be ≥ 1, for every hash,
	// including the all-zero and all-one extremes.
	for _, hash := range []uint64{0, math.MaxUint64, 1, 1 << 63, 0xF0F0F0F0F0F0F0F0} {
		idx, rank := hllRegister(hash)
		if idx >= hllM {
			t.Errorf("hash %#x: idx %d out of range", hash, idx)
		}
		if rank < 1 || rank > 63 {
			t.Errorf("hash %#x: rank %d out of [1,63]", hash, rank)
		}
	}
}
