// Package rmat implements the RMAT (Recursive MATrix) generator
// of Chakrabarti, Zhan & Faloutsos (SDM 2004), used to produce
// power-law-shaped synthetic graphs that match the degree
// distributions observed in real-world social / web networks.
//
// The four parameters A, B, C, D sum to 1.0; their relative
// proportions control the graph's degree distribution. The
// canonical defaults are (0.57, 0.19, 0.19, 0.05), matching the
// Graph500 specification.
package rmat

import (
	"math/rand/v2"

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
func Generate(spec Spec, loader *bulk.Loader) (vertices, edges uint64) {
	if spec.A == 0 && spec.B == 0 && spec.C == 0 && spec.D == 0 {
		spec.A, spec.B, spec.C, spec.D = 0.57, 0.19, 0.19, 0.05
	}
	if spec.EdgeFactor <= 0 {
		spec.EdgeFactor = 16
	}
	n := uint64(1) << spec.Scale
	m := uint64(spec.EdgeFactor) * n
	r := rand.New(rand.NewPCG(spec.Seed, 0xA5A5A5A5A5A5A5A5)) //nolint:gosec // deterministic generator
	ab := spec.A + spec.B
	abc := ab + spec.C
	for i := uint64(0); i < m; i++ {
		src, dst := pick(r, n, ab, abc, spec.A+spec.B+spec.C+spec.D)
		loader.Add(bulk.Edge{Src: itoa(src), Dst: itoa(dst), Weight: 1})
	}
	return n, m
}

func pick(r *rand.Rand, n uint64, ab, abc, total float64) (srcOut, dstOut uint64) {
	for size := n; size > 1; size /= 2 {
		half := size / 2
		v := r.Float64() * total
		switch {
		case v < ab:
			if v >= ab-(ab-0)/2 { // B region (right top quadrant)
				dstOut += half
			}
		case v < abc:
			srcOut += half
		default:
			srcOut += half
			dstOut += half
		}
	}
	return srcOut, dstOut
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
