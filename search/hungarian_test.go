package search

import (
	"errors"
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
	a, err := Hungarian(cost, 3, 3)
	if err != nil {
		t.Fatalf("Hungarian: %v", err)
	}
	if math.Abs(a.TotalCost-5) > 1e-9 {
		t.Fatalf("TotalCost = %f, want 5", a.TotalCost)
	}
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
	cost := []float64{
		3, 0, 7,
		2, 5, 0,
	}
	a, err := Hungarian(cost, 2, 3)
	if err != nil {
		t.Fatalf("Hungarian: %v", err)
	}
	if a.TotalCost != 0 {
		t.Fatalf("TotalCost = %f, want 0", a.TotalCost)
	}
}

func TestHungarian_Empty(t *testing.T) {
	t.Parallel()
	a, err := Hungarian([]float64{}, 0, 0)
	if err != nil {
		t.Fatalf("empty case: %v", err)
	}
	if a.TotalCost != 0 || len(a.RowToCol) != 0 {
		t.Fatalf("empty case: %+v", a)
	}
}

// TestHungarian_RejectsNaN asserts that any NaN entry in the cost
// matrix is rejected with ErrInvalidInput; without this guard the
// dual-potential accumulation propagates NaN silently and corrupts
// the result.
func TestHungarian_RejectsNaN(t *testing.T) {
	t.Parallel()
	cost := []float64{1, 2, math.NaN(), 4}
	_, err := Hungarian(cost, 2, 2)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestHungarian_RejectsInf(t *testing.T) {
	t.Parallel()
	cost := []float64{1, 2, math.Inf(1), 4}
	_, err := Hungarian(cost, 2, 2)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestHungarian_RejectsMismatchedLength(t *testing.T) {
	t.Parallel()
	cost := []float64{1, 2, 3}
	_, err := Hungarian(cost, 2, 2)
	if err == nil {
		t.Fatal("expected length-mismatch error")
	}
}

