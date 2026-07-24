package stats

import (
	"math"
	"math/bits"
)

// HLL parameters. p is the register-index precision; m = 2^p is the register
// count. p = 12 gives m = 4096 registers and a relative standard error of
// 1.04/√m ≈ 1.625% (design docs/statistics-design.md §1).
const (
	hllP        = 12
	hllM        = 1 << hllP             // 4096 registers
	hllRegBits  = 6                     // a register holds a rank in [0,63]; 6 bits suffice
	hllDenseLen = hllM * hllRegBits / 8 // 3072 bytes, the packed dense footprint
)

// hllSparseMax is the number of distinct touched registers at or above which the
// sparse representation is promoted to the dense packed array. Below it the
// estimator keeps an exact register map and estimates by linear counting, which
// is far more accurate than the raw HLL estimator for small cardinalities (the
// low-cardinality correction the HLL++ sparse mode exists to provide). The
// threshold is a quarter of m: comfortably inside the linear-counting-accurate
// regime, and the map footprint stays well under the 3 KiB dense array it
// replaces.
const hllSparseMax = hllM / 4

// HLL is a HyperLogLog++ distinct-value (cardinality) estimator over 64-bit
// hashes. It starts in a sparse, exact-per-register representation for small
// cardinalities and promotes to a 6-bit-packed dense array of m = 4096 registers
// once enough distinct registers are touched. Small cardinalities are estimated
// by linear counting (guarded against a zero empty-register count); larger ones
// by Ertl's table-free improved register-histogram estimator, whose exact-dyadic
// fold uses no math.Pow. See [HLL.Estimate] for the full rationale.
//
// The zero value is not usable; construct one with [NewHLL]. An HLL is NOT safe
// for concurrent Insert; once fully built it is immutable and safe for concurrent
// [HLL.Estimate] reads. Insert is called only off the write path, during a
// statistics rebuild scan.
type HLL struct {
	// sparse maps a touched register index to its current maximum rank. It is the
	// active representation while len(sparse) < hllSparseMax; promotion to dense
	// nils it out.
	sparse map[uint16]uint8
	// dense is the 6-bit-packed register array, non-nil only after promotion.
	dense []byte
}

// NewHLL returns an empty estimator in the sparse representation.
func NewHLL() *HLL {
	return &HLL{sparse: make(map[uint16]uint8)}
}

// hllRegister splits a 64-bit hash into the register index (top p bits) and the
// rank ρ = 1 + the number of leading zeros of the remaining 64−p bits. A sentinel
// bit below the remainder guarantees a non-zero word, so ρ is bounded by 64−p+1
// (≤ 53 here) and always fits in the 6-bit register.
func hllRegister(hash uint64) (idx uint16, rank uint8) {
	idx = uint16(hash >> (64 - hllP))
	// Shift the p index bits off the top, then set a sentinel bit just below the
	// 64−p remainder region so LeadingZeros64 is bounded and never counts the
	// (now-zero) low index bits.
	w := (hash << hllP) | (1 << (hllP - 1))
	rank = uint8(bits.LeadingZeros64(w) + 1)
	return idx, rank
}

// hllMix is the MurmurHash3 fmix64 finaliser: a bijection with full avalanche.
// Insert applies it so the estimator is robust to any equality-consistent input
// hash, even one whose high bits are near-constant. The Cypher layer's
// expr.EquivalentHash is optimised for hash-bucket distribution (its low bits),
// so its HIGH bits — which HLL uses for the register index — are nearly identical
// for small floats (their shared exponent field), which without this finaliser
// would collapse thousands of distinct values onto a handful of registers and
// wreck the estimate. Being a bijection, it preserves both the equivalence
// property (equal hash → equal register) and Insert's idempotency.
func hllMix(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// Insert folds a 64-bit hash into the estimator. Callers pass any hash that is
// consistent with their equality relation (the Cypher layer uses
// expr.EquivalentHash so numerically-equal Integer/Float values, ±0.0, and all
// NaN bit-patterns fold to one register); Insert finalises it internally
// ([hllMix]) so the register distribution is uniform regardless. Insert only ever
// raises a register, so it is monotonic and idempotent for a repeated hash.
func (h *HLL) Insert(hash uint64) {
	idx, rank := hllRegister(hllMix(hash))
	if h.dense != nil {
		if h.denseGet(idx) < rank {
			h.denseSet(idx, rank)
		}
		return
	}
	if cur, ok := h.sparse[idx]; !ok || cur < rank {
		h.sparse[idx] = rank
	}
	if len(h.sparse) >= hllSparseMax {
		h.promote()
	}
}

// promote converts the sparse representation to the dense packed array and
// discards the map.
func (h *HLL) promote() {
	h.dense = make([]byte, hllDenseLen)
	for idx, rank := range h.sparse {
		if h.denseGet(idx) < rank {
			h.denseSet(idx, rank)
		}
	}
	h.sparse = nil
}

// denseGet reads the 6-bit register idx from the packed array. The register
// occupies bits [6·idx, 6·idx+6) of the little-endian byte stream, which spans at
// most two adjacent bytes.
func (h *HLL) denseGet(idx uint16) uint8 {
	bitPos := uint(idx) * hllRegBits
	bytePos := bitPos >> 3
	shift := bitPos & 7
	// Read two bytes as a 16-bit little-endian window and extract the 6-bit field.
	lo := uint16(h.dense[bytePos])
	var hi uint16
	if bytePos+1 < uint(len(h.dense)) {
		hi = uint16(h.dense[bytePos+1])
	}
	win := lo | hi<<8
	return uint8((win >> shift) & 0x3F)
}

// denseSet writes the 6-bit value v into register idx of the packed array.
func (h *HLL) denseSet(idx uint16, v uint8) {
	bitPos := uint(idx) * hllRegBits
	bytePos := bitPos >> 3
	shift := bitPos & 7
	mask := uint16(0x3F) << shift
	win := uint16(h.dense[bytePos])
	if bytePos+1 < uint(len(h.dense)) {
		win |= uint16(h.dense[bytePos+1]) << 8
	}
	win = (win &^ mask) | (uint16(v) << shift)
	h.dense[bytePos] = byte(win)
	if bytePos+1 < uint(len(h.dense)) {
		h.dense[bytePos+1] = byte(win >> 8)
	}
}

// hllQ is the largest rank a register can hold: 64−p (=52), so a register value
// ranges over [0, hllQ+1] (0 = empty, hllQ+1 = the capped maximum rank). It bounds
// the register histogram the dense estimator folds.
const hllQ = 64 - hllP

// hllAlphaInf is α_∞ = 1/(2·ln 2), the limiting bias constant used by the
// register-histogram estimator (Ertl 2017, "New cardinality estimation algorithms
// for HyperLogLog sketches").
var hllAlphaInf = 0.5 / math.Ln2

// Estimate returns the estimated number of distinct values folded into the
// receiver.
//
// Small cardinalities (the sparse representation) use linear counting
// m·ln(m/V) over the empty-register count V, which is near-exact in that regime;
// the m·ln(m/V) term is guarded against V = 0 (design docs/statistics-design.md
// §5.1). Larger cardinalities (the dense representation) use Ertl's table-free
// improved register-histogram estimator, which realises the HLL++ small-and-mid-
// range bias correction the spec requires WITHOUT any precision-specific empirical
// bias tables to transcribe: the classic harmonic-mean-plus-linear-counting
// estimator is measurably outside the ±2·(1.04/√m) accuracy band in the 2m–5m
// transition region (the very reason HLL++ adds a bias correction), whereas Ertl's
// closed form holds within one standard error across the entire range and is
// verifiable against a single published algorithm.
//
// The estimator's core is the register-value histogram folded as z = 0.5·(z +
// c_k) down the ranks — the numerically-stable, exact-dyadic evaluation of the
// harmonic-mean sum Σ_k c_k·2^{−k} (each ×0.5 is exact in IEEE-754, so no
// math.Pow ever appears, per §5.1) — bracketed by the σ (empty-register) and τ
// (saturated-register) tail corrections that make it accurate at both extremes.
func (h *HLL) Estimate() float64 {
	if h.dense == nil {
		// Sparse: V empty registers = m − (distinct touched). Linear counting.
		nonEmpty := len(h.sparse)
		if nonEmpty == 0 {
			return 0
		}
		v := hllM - nonEmpty
		if v == 0 {
			// Fully saturated while still sparse is unreachable (promotion happens
			// long before), but guard the log anyway.
			return float64(hllM)
		}
		return linearCounting(v)
	}

	// Dense: build the register-value histogram c_k (c_0 = empty registers,
	// c_{hllQ+1} = registers at the capped maximum rank).
	var hist [hllQ + 2]int
	for idx := 0; idx < hllM; idx++ {
		hist[h.denseGet(uint16(idx))]++
	}

	m := float64(hllM)
	// τ tail correction for the saturated registers.
	z := m * hllTau((m-float64(hist[hllQ+1]))/m)
	// Fold the register histogram from the highest rank down: z = 0.5·(z + c_k).
	// This is the exact-dyadic harmonic-mean sum (no math.Pow).
	for k := hllQ; k >= 1; k-- {
		z = 0.5 * (z + float64(hist[k]))
	}
	// σ tail correction for the empty registers.
	z += m * hllSigma(float64(hist[0])/m)
	return hllAlphaInf * m * m / z
}

// hllSigma is the σ helper of Ertl's estimator, a rapidly-convergent series that
// corrects for empty registers. σ(1) = +∞ (every register empty), which drives
// the estimate to 0 through the α·m²/z quotient.
func hllSigma(x float64) float64 {
	if x == 1.0 {
		return math.Inf(1)
	}
	y := 1.0
	z := x
	for {
		x *= x
		prev := z
		z += x * y
		y += y
		if z == prev {
			return z
		}
	}
}

// hllTau is the τ helper of Ertl's estimator, correcting for registers at the
// capped maximum rank. τ(0) = τ(1) = 0. It squares-roots x each step (never
// math.Pow) and uses (1−x)·(1−x) for the squared term.
func hllTau(x float64) float64 {
	if x == 0.0 || x == 1.0 {
		return 0.0
	}
	y := 1.0
	z := 1.0 - x
	for {
		x = math.Sqrt(x)
		prev := z
		y *= 0.5
		z -= (1 - x) * (1 - x) * y
		if z == prev {
			return z
		}
	}
}

// linearCounting returns m·ln(m/V) for V > 0 empty registers. The caller
// guarantees V > 0; the divide-and-log is therefore well-defined.
func linearCounting(v int) float64 {
	return float64(hllM) * math.Log(float64(hllM)/float64(v))
}

// Bytes reports the estimator's current register footprint in bytes: the packed
// dense array once promoted, or an approximation of the sparse map's live entries
// beforehand. It surfaces the memory the estimator holds for observability.
func (h *HLL) Bytes() int {
	if h.dense != nil {
		return len(h.dense)
	}
	// Sparse map: 3 bytes of live payload per entry (uint16 key + uint8 value);
	// map bookkeeping is not counted, matching the dense figure's payload-only basis.
	return len(h.sparse) * 3
}
