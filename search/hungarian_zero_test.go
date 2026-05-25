package search

import (
	"math"
	"testing"
)

// makeRandomCost10 builds a deterministic 10x10 cost matrix with values
// in [1, 99] using a simple LCG so the tests do not depend on the
// rand package import that is already pulled in by other files.
func makeRandomCost10(seed uint64) []float64 {
	cost := make([]float64, 100)
	v := seed
	for i := range cost {
		v = v*6364136223846793005 + 1442695040888963407 // Knuth LCG
		cost[i] = float64(v>>57) + 1                    // 1..128, skip 0
	}
	return cost
}

// validateSquareAssignment checks that asgn is a valid permutation
// for an n×n matrix and returns the total cost.
func validateSquareAssignment(t *testing.T, label string, cost []float64, n int, asgn Assignment) float64 {
	t.Helper()
	if len(asgn.RowToCol) != n {
		t.Fatalf("%s: RowToCol length = %d, want %d", label, len(asgn.RowToCol), n)
	}
	seen := make(map[int]bool, n)
	for row, col := range asgn.RowToCol {
		if col < 0 {
			t.Fatalf("%s: row %d unassigned in square matrix", label, row)
		}
		if seen[col] {
			t.Fatalf("%s: column %d assigned to two rows", label, col)
		}
		seen[col] = true
	}
	return assignmentCost(cost, asgn, n, n)
}

// TestHungarian_ZeroRowAndColumn checks that Hungarian produces a
// valid minimum assignment when a cost matrix contains one or more
// zero rows, zero columns, or is entirely zero.
func TestHungarian_ZeroRowAndColumn(t *testing.T) {
	t.Parallel()
	const n = 10

	base := makeRandomCost10(0xdeadbeef)

	t.Run("zero_row_0", func(t *testing.T) {
		t.Parallel()
		cost := make([]float64, n*n)
		copy(cost, base)
		// Zero out row 0.
		for j := 0; j < n; j++ {
			cost[0*n+j] = 0
		}
		asgn, err := Hungarian(cost, n, n)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := validateSquareAssignment(t, "zero_row_0", cost, n, asgn)
		bf := bruteForceMin(cost, n)
		if math.Abs(got-bf) > 1e-9 {
			t.Errorf("cost %g != brute-force min %g", got, bf)
		}
	})

	t.Run("zero_col_0", func(t *testing.T) {
		t.Parallel()
		cost := make([]float64, n*n)
		copy(cost, base)
		// Zero out column 0.
		for i := 0; i < n; i++ {
			cost[i*n+0] = 0
		}
		asgn, err := Hungarian(cost, n, n)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := validateSquareAssignment(t, "zero_col_0", cost, n, asgn)
		bf := bruteForceMin(cost, n)
		if math.Abs(got-bf) > 1e-9 {
			t.Errorf("cost %g != brute-force min %g", got, bf)
		}
	})

	t.Run("zero_row_and_col", func(t *testing.T) {
		t.Parallel()
		cost := make([]float64, n*n)
		copy(cost, base)
		// Zero out row 0 and column 0.
		for j := 0; j < n; j++ {
			cost[0*n+j] = 0
		}
		for i := 0; i < n; i++ {
			cost[i*n+0] = 0
		}
		asgn, err := Hungarian(cost, n, n)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := validateSquareAssignment(t, "zero_row_and_col", cost, n, asgn)
		bf := bruteForceMin(cost, n)
		if math.Abs(got-bf) > 1e-9 {
			t.Errorf("cost %g != brute-force min %g", got, bf)
		}
	})

	t.Run("all_zeros", func(t *testing.T) {
		t.Parallel()
		cost := make([]float64, n*n) // zero-valued by default
		asgn, err := Hungarian(cost, n, n)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := validateSquareAssignment(t, "all_zeros", cost, n, asgn)
		if math.Abs(got) > 1e-9 {
			t.Errorf("all-zero matrix: TotalCost = %g, want 0", got)
		}
	})
}
