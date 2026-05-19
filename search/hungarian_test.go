package search

import (
	"math"
	"testing"
)

func TestHungarian_Square(t *testing.T) {
	t.Parallel()
	// 3x3 cost matrix. Brute-force optimum (over all 6 permutations)
	// is 1 + 2 + 2 = 5, picking columns (1, 0, 2).
	cost := []float64{
		4, 1, 3,
		2, 0, 5,
		3, 2, 2,
	}
	a := Hungarian(cost, 3, 3)
	if math.Abs(a.TotalCost-5) > 1e-9 {
		t.Fatalf("TotalCost = %f, want 5", a.TotalCost)
	}
	// Each row must map to a distinct column.
	seen := map[int]bool{}
	for _, c := range a.RowToCol {
		if c < 0 {
			t.Fatalf("RowToCol contains -1: %v", a.RowToCol)
		}
		if seen[c] {
			t.Fatalf("duplicate column %d", c)
		}
		seen[c] = true
	}
}

func TestHungarian_Rectangular(t *testing.T) {
	t.Parallel()
	// 2 rows, 3 columns. Optimum = 0+0 = 0 by picking the zero in
	// each row.
	cost := []float64{
		3, 0, 7,
		2, 5, 0,
	}
	a := Hungarian(cost, 2, 3)
	if a.TotalCost != 0 {
		t.Fatalf("TotalCost = %f, want 0", a.TotalCost)
	}
}

func TestHungarian_Empty(t *testing.T) {
	t.Parallel()
	a := Hungarian([]float64{}, 0, 0)
	if a.TotalCost != 0 || len(a.RowToCol) != 0 {
		t.Fatalf("empty case: %+v", a)
	}
}
