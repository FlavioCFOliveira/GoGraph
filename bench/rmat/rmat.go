// Package rmat implements the RMAT (Recursive MATrix) generator
// of Chakrabarti, Zhan & Faloutsos (SDM 2004), used to produce
// power-law-shaped synthetic graphs that match the degree
// distributions observed in real-world social / web networks.
//
// The four parameters A, B, C, D sum to 1.0; their relative
// proportions control the graph's degree distribution. The
// canonical defaults are (0.57, 0.19, 0.19, 0.05), matching the
// Graph500 specification.
//
// The recursive descent through the 2x2 probability matrix is
// shared with the catalogue generator at
// [gograph/internal/shapegen.RMAT] via [shapegen.RMATPick]: both
// callers run the exact same Graph500 quadrant split (A iff
// v < a, B iff a <= v < a+b, C iff a+b <= v < a+b+c, D otherwise)
// in integer-percent arithmetic. The previous bench/rmat
// implementation split A from B at ab/2 instead of at a, which is
// correct only when a == b and silently diverged for the canonical
// (57, 19, 19, 5) tuple; sharing the picker fixes that bug at the
// source.
package rmat

import (
	"fmt"
	"math"
	"math/rand/v2"

	"gograph/internal/shapegen"
	"gograph/store/bulk"
)

// Spec configures a [Generate] run.
type Spec struct {
	// Scale = log2(n_vertices). The generated graph has 2^Scale
	// nodes.
	Scale int
	// EdgeFactor is the average degree; total edges = EdgeFactor *
	// 2^Scale.
	EdgeFactor int
	// A, B, C, D are the RMAT quadrant probabilities; sum to 1.
	// Defaults: 0.57 / 0.19 / 0.19 / 0.05 when zero.
	A, B, C, D float64
	// Seed is the PCG seed for reproducibility.
	Seed uint64
}

// DefaultSpec returns the canonical Graph500 RMAT preset.
func DefaultSpec() Spec {
	return Spec{Scale: 10, EdgeFactor: 16, A: 0.57, B: 0.19, C: 0.19, D: 0.05, Seed: 1}
}

// Generate streams the RMAT edges into the supplied bulk.Loader.
// Returns the number of vertices and edges produced.
//
// The picker delegates to [shapegen.RMATPick] so this bench shares
// its quadrant-split semantics with the catalogue generator. The
// caller-supplied (A, B, C, D) are rounded to the nearest integer
// percent in [0, 100]; the sum of the rounded percents must equal
// 100 or Generate panics with a deterministic, self-describing
// message. The all-zero case is treated as the canonical Graph500
// default (0.57, 0.19, 0.19, 0.05) for backwards compatibility with
// the v1 API.
//
//nolint:gosec // G404: math/rand/v2 is the pinned PRNG for benchmark determinism.
func Generate(spec Spec, loader *bulk.Loader) (vertices, edges uint64) {
	if spec.A == 0 && spec.B == 0 && spec.C == 0 && spec.D == 0 {
		spec.A, spec.B, spec.C, spec.D = 0.57, 0.19, 0.19, 0.05
	}
	if spec.EdgeFactor <= 0 {
		spec.EdgeFactor = 16
	}
	a, b, c := percentsFromSpec(spec)
	ab := a + b
	abc := ab + c
	n := uint64(1) << spec.Scale
	m := uint64(spec.EdgeFactor) * n
	r := rand.New(rand.NewPCG(spec.Seed, 0xA5A5A5A5A5A5A5A5))
	for i := uint64(0); i < m; i++ {
		src, dst := shapegen.RMATPick(r, n, a, ab, abc)
		_ = loader.Add(bulk.Edge{Src: itoa(src), Dst: itoa(dst), Weight: 1})
	}
	return n, m
}

// percentsFromSpec rounds (A, B, C, D) to integer percents in
// [0, 100] and returns (a, b, c). The d component is implied at
// the (100 - a - b - c) boundary and is therefore omitted from the
// return tuple, matching the [shapegen.RMATPick] signature.
//
// percentsFromSpec panics when:
//
//   - any of A, B, C, D rounds to a value outside [0, 100];
//   - the four rounded components do not sum to 100.
//
// The panic message is deterministic and includes both the original
// floats and their rounded values, so the caller can diagnose
// rounding drift without re-running.
func percentsFromSpec(spec Spec) (a, b, c int) {
	a = roundPercent(spec.A, "A")
	b = roundPercent(spec.B, "B")
	c = roundPercent(spec.C, "C")
	d := roundPercent(spec.D, "D")
	if a+b+c+d != 100 {
		panic(fmt.Sprintf(
			"bench/rmat: rounded (A, B, C, D) = (%d, %d, %d, %d) does not sum to 100 (raw floats = (%g, %g, %g, %g))",
			a, b, c, d, spec.A, spec.B, spec.C, spec.D,
		))
	}
	return a, b, c
}

// roundPercent rounds x*100 to the nearest int and panics if the
// result falls outside [0, 100]. The label is embedded in the panic
// message so the offending field is unambiguous.
func roundPercent(x float64, label string) int {
	v := int(math.Round(x * 100))
	if v < 0 || v > 100 {
		panic(fmt.Sprintf("bench/rmat: %s = %g rounds to %d percent, outside [0, 100]", label, x, v))
	}
	return v
}

// itoa is a small ad-hoc base-10 formatter (we want zero-alloc on
// the generator hot path; fmt.Sprintf would allocate).
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
