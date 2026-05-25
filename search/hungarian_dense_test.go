package search

import (
	"math"
	"math/rand/v2"
	"testing"
)

// bruteForceMin computes the minimum assignment cost over all n!
// permutations. Only feasible for small n (up to ~8).
func bruteForceMin(cost []float64, n int) float64 {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	best := math.MaxFloat64
	for {
		total := 0.0
		for row, col := range perm {
			total += cost[row*n+col]
		}
		if total < best {
			best = total
		}
		if !nextPermutation(perm) {
			break
		}
	}
	return best
}

// nextPermutation advances perm to the next lexicographic permutation
// in-place. Returns false when perm was the last permutation.
func nextPermutation(perm []int) bool {
	n := len(perm)
	i := n - 2
	for i >= 0 && perm[i] >= perm[i+1] {
		i--
	}
	if i < 0 {
		return false
	}
	j := n - 1
	for perm[j] <= perm[i] {
		j--
	}
	perm[i], perm[j] = perm[j], perm[i]
	for l, ri := i+1, n-1; l < ri; l, ri = l+1, ri-1 {
		perm[l], perm[ri] = perm[ri], perm[l]
	}
	return true
}

// assignmentCost returns the total cost of an assignment for an n*m
// cost matrix (row-major). asgn.RowToCol[r] is the column assigned to
// row r; -1 means unassigned.
func assignmentCost(cost []float64, asgn Assignment, n, m int) float64 {
	total := 0.0
	for row, col := range asgn.RowToCol {
		if col < 0 {
			continue
		}
		total += cost[row*m+col]
	}
	return total
}

// randomCostMatrix generates an n*n cost matrix with values in [0, maxVal)
// using the given seed. Each call gets its own RNG so no concurrency hazard.
func randomCostMatrix(n, maxVal int, seed1, seed2 uint64) []float64 {
	r := rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // deterministic test RNG
	cost := make([]float64, n*n)
	for i := range cost {
		cost[i] = float64(r.IntN(maxVal))
	}
	return cost
}

// TestHungarian_DenseRandom cross-checks Hungarian against brute-force
// permutation search for small n, and verifies a valid assignment for
// larger n.
func TestHungarian_DenseRandom(t *testing.T) {
	t.Parallel()

	// Small n: cross-check with brute force.
	// Each subtest generates its own cost matrix with a unique seed
	// to avoid any shared-state race under t.Parallel().
	for idx, n := range []int{3, 5, 8} {
		n, seed := n, uint64(idx+1)
		t.Run("brute_force_n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			cost := randomCostMatrix(n, 100, seed*42, seed*43)
			asgn, err := Hungarian(cost, n, n)
			if err != nil {
				t.Fatalf("n=%d: Hungarian error: %v", n, err)
			}
			if len(asgn.RowToCol) != n {
				t.Fatalf("n=%d: RowToCol length = %d, want %d", n, len(asgn.RowToCol), n)
			}
			// Verify valid permutation (no column assigned twice).
			seen := make(map[int]bool, n)
			for _, col := range asgn.RowToCol {
				if col < 0 {
					t.Fatalf("n=%d: unassigned row in square matrix", n)
				}
				if seen[col] {
					t.Fatalf("n=%d: column %d assigned twice", n, col)
				}
				seen[col] = true
			}
			got := assignmentCost(cost, asgn, n, n)
			bfMin := bruteForceMin(cost, n)
			if math.Abs(got-bfMin) > 1e-9 {
				t.Errorf("n=%d: Hungarian cost %g != brute-force min %g", n, got, bfMin)
			}
		})
	}

	// Larger n: verify valid permutation (no brute force; too slow).
	for idx, n := range []int{10, 20} {
		n, seed := n, uint64(idx+100)
		t.Run("valid_assignment_n="+itoa(n), func(t *testing.T) {
			t.Parallel()
			cost := randomCostMatrix(n, 1000, seed*42, seed*43)
			asgn, err := Hungarian(cost, n, n)
			if err != nil {
				t.Fatalf("n=%d: Hungarian error: %v", n, err)
			}
			if len(asgn.RowToCol) != n {
				t.Fatalf("n=%d: RowToCol length = %d, want %d", n, len(asgn.RowToCol), n)
			}
			seen := make(map[int]bool, n)
			for row, col := range asgn.RowToCol {
				if col < 0 {
					t.Fatalf("n=%d: row %d unassigned in square matrix", n, row)
				}
				if seen[col] {
					t.Fatalf("n=%d: column %d assigned twice", n, col)
				}
				seen[col] = true
			}
			// All costs are non-negative; total must be non-negative too.
			if asgn.TotalCost < 0 {
				t.Errorf("n=%d: negative TotalCost %g", n, asgn.TotalCost)
			}
		})
	}
}
