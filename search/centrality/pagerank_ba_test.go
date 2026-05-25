package centrality

import (
	"math"
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestPageRank_BarabasiAlbert runs PageRank on a Barabási–Albert
// preferential-attachment graph and verifies that high-degree hubs
// concentrate rank, measured via Spearman correlation and top-k set
// intersection.
func TestPageRank_BarabasiAlbert(t *testing.T) {
	t.Parallel()

	g, err := shapegen.BarabasiAlbert(2000, 3, 42).Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	opts := PageRankOptions{
		Damping:       0.85,
		MaxIterations: 200,
		Tolerance:     1e-8,
	}
	ranks, _, err := PageRank(c, opts)
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}

	verts := c.VerticesSlice()
	n := len(verts) - 1

	// Build per-node degree and PageRank slices over live nodes only.
	liveMask := c.LiveMask()

	type nodeVal struct {
		id     int
		degree float64
		rank   float64
	}
	nodes := make([]nodeVal, 0, n)
	for i := 0; i < n; i++ {
		if i < len(liveMask) && !liveMask[i] {
			continue
		}
		deg := float64(verts[i+1] - verts[i])
		if deg == 0 && (i >= len(liveMask) || !liveMask[i]) {
			continue
		}
		nodes = append(nodes, nodeVal{id: i, degree: deg, rank: ranks[i]})
	}

	m := len(nodes)
	if m < 10 {
		t.Fatalf("too few live nodes: %d", m)
	}

	degVec := make([]float64, m)
	rkVec := make([]float64, m)
	for i, nv := range nodes {
		degVec[i] = nv.degree
		rkVec[i] = nv.rank
	}

	// 1. Spearman correlation between degree and PageRank >= 0.85.
	rho := spearmanCorrelation(degVec, rkVec)
	if rho < 0.85 {
		t.Fatalf("Spearman(degree, PageRank) = %.4f, want >= 0.85", rho)
	}

	// 2. Top-10 by PageRank intersects top-20 by degree at >= 8 vertices.
	const topN = 10
	const topDeg = 20

	byRank := make([]nodeVal, m)
	copy(byRank, nodes)
	sort.Slice(byRank, func(i, j int) bool { return byRank[i].rank > byRank[j].rank })

	byDeg := make([]nodeVal, m)
	copy(byDeg, nodes)
	sort.Slice(byDeg, func(i, j int) bool { return byDeg[i].degree > byDeg[j].degree })

	top10IDs := make(map[int]bool, topN)
	for i := 0; i < topN && i < len(byRank); i++ {
		top10IDs[byRank[i].id] = true
	}
	top20IDs := make(map[int]bool, topDeg)
	for i := 0; i < topDeg && i < len(byDeg); i++ {
		top20IDs[byDeg[i].id] = true
	}
	var overlap int
	for id := range top10IDs {
		if top20IDs[id] {
			overlap++
		}
	}
	if overlap < 8 {
		t.Fatalf("top-10 PageRank ∩ top-20 degree = %d, want >= 8", overlap)
	}

	// 3. Mass conservation within tolerance.
	var totalMass float64
	for _, r := range ranks {
		totalMass += r
	}
	if math.Abs(totalMass-1.0) > 1e-8 {
		t.Fatalf("mass sum = %.15f, want 1.0 (delta %.3g)", totalMass, math.Abs(totalMass-1.0))
	}
}

// spearmanCorrelation returns the Spearman rank correlation of x and y.
// Ties receive average ranks. Both slices must have the same length.
func spearmanCorrelation(x, y []float64) float64 {
	n := len(x)
	if n == 0 {
		return 0
	}
	rx := rankVector(x)
	ry := rankVector(y)
	return pearsonCorrelation(rx, ry)
}

// rankVector converts a float64 slice to its rank vector.
// Equal values receive the average of their tied ranks.
func rankVector(v []float64) []float64 {
	n := len(v)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return v[idx[i]] < v[idx[j]] })

	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i + 1
		for j < n && v[idx[j]] == v[idx[i]] {
			j++
		}
		avgRank := float64(i+j+1) / 2.0 // average of 1-based ranks i+1 .. j
		for k := i; k < j; k++ {
			ranks[idx[k]] = avgRank
		}
		i = j
	}
	return ranks
}

// pearsonCorrelation returns the Pearson correlation of x and y.
func pearsonCorrelation(x, y []float64) float64 {
	n := float64(len(x))
	if n == 0 {
		return 0
	}
	var sumX, sumY, sumXX, sumYY, sumXY float64
	for i := range x {
		sumX += x[i]
		sumY += y[i]
		sumXX += x[i] * x[i]
		sumYY += y[i] * y[i]
		sumXY += x[i] * y[i]
	}
	num := n*sumXY - sumX*sumY
	den := math.Sqrt((n*sumXX - sumX*sumX) * (n*sumYY - sumY*sumY))
	if den == 0 {
		return 0
	}
	return num / den
}
