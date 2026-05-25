//go:build soak

package extern

import (
	"math"
	"path/filepath"
	"sort"
	"testing"

	"gograph/bench/rmat"
	"gograph/internal/testlayers"
	"gograph/store/bulk"
	"gograph/store/csrfile"
)

// TestPageRank_RMATScale20_Soak verifies extern.PageRank on a
// RMAT-scale-20 graph (~1M vertices, ~8M edges):
//   - no error
//   - rank sum within 1e-6 of 1.0
//   - top-10 nodes have rank > 2x the mean
//   - Spearman correlation between out-degree and PageRank >= 0.5
func TestPageRank_RMATScale20_Soak(t *testing.T) {
	testlayers.RequireSoak(t)

	path := filepath.Join(t.TempDir(), "rmat20_pr.csr")
	loader := bulk.New(bulk.Options{OutputPath: path, Directed: true})
	rmat.Generate(rmat.Spec{
		Scale:      20,
		EdgeFactor: 8,
		A:          0.57,
		B:          0.19,
		C:          0.19,
		D:          0.05,
		Seed:       42,
	}, loader)
	if _, _, err := loader.Finalise(); err != nil {
		t.Fatalf("loader.Finalise: %v", err)
	}

	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	ranks, _, err := PageRank(r, DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}

	// --- rank sum ---
	var total float64
	for _, v := range ranks {
		total += v
	}
	if math.Abs(total-1.0) > 1e-6 {
		t.Fatalf("rank sum = %.9f, want 1.0 (±1e-6)", total)
	}

	// --- top-10 nodes significantly above mean ---
	n := len(ranks)
	if n == 0 {
		t.Fatal("ranks slice is empty")
	}
	mean := total / float64(n)

	sorted := make([]float64, n)
	copy(sorted, ranks)
	sort.Float64s(sorted)
	// Top-10 are at the high end of the sorted slice.
	top10Start := n - 10
	if top10Start < 0 {
		top10Start = 0
	}
	for i := top10Start; i < n; i++ {
		if sorted[i] <= 2*mean {
			t.Errorf("top-10 rank[%d] = %.9f not > 2x mean (%.9f)", i, sorted[i], mean)
		}
	}

	// --- Spearman(out-degree, PageRank) >= 0.5 ---
	verts := r.Vertices()
	nv := len(verts) - 1 // number of vertices

	outDeg := make([]float64, nv)
	for i := 0; i < nv; i++ {
		outDeg[i] = float64(verts[i+1] - verts[i])
	}

	ranksForCorr := ranks
	if len(ranksForCorr) > nv {
		ranksForCorr = ranks[:nv]
	}
	corr := spearmanRankCorrelation(outDeg, ranksForCorr)
	if corr < 0.5 {
		t.Errorf("Spearman(out-degree, PageRank) = %.4f, want >= 0.5", corr)
	}
}

// spearmanRankCorrelation computes the Spearman rank correlation
// coefficient of x and y, which must have the same length.
// It ranks both slices and then computes their Pearson correlation.
func spearmanRankCorrelation(x, y []float64) float64 {
	n := len(x)
	if n != len(y) || n == 0 {
		return 0
	}
	rx := rankSlice(x)
	ry := rankSlice(y)
	return pearsonCorrelation(rx, ry)
}

// rankSlice assigns rank 1..n to each element of v (ties receive the
// average of the ranks they would occupy). Returns the rank vector.
func rankSlice(v []float64) []float64 {
	n := len(v)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })

	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i + 1
		for j < n && v[idx[j]] == v[idx[i]] {
			j++
		}
		// Positions i..j-1 (1-indexed: i+1..j) share the average rank.
		avg := float64(i+1+j) / 2.0
		for k := i; k < j; k++ {
			ranks[idx[k]] = avg
		}
		i = j
	}
	return ranks
}

// pearsonCorrelation computes the Pearson correlation of a and b.
func pearsonCorrelation(a, b []float64) float64 {
	n := float64(len(a))
	if n == 0 {
		return 0
	}
	var sumA, sumB, sumAB, sumA2, sumB2 float64
	for i := range a {
		sumA += a[i]
		sumB += b[i]
		sumAB += a[i] * b[i]
		sumA2 += a[i] * a[i]
		sumB2 += b[i] * b[i]
	}
	num := sumAB - sumA*sumB/n
	denomA := sumA2 - sumA*sumA/n
	denomB := sumB2 - sumB*sumB/n
	if denomA <= 0 || denomB <= 0 {
		return 0
	}
	return num / math.Sqrt(denomA*denomB)
}
