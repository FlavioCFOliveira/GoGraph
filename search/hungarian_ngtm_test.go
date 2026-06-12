package search

import (
	"context"
	"errors"
	"testing"
)

// TestHungarian_NGreaterThanM is the mandatory gate test for the n>m hang fix.
//
// Before the fix, Hungarian([]float64{1,2,3,4,5,6}, 3, 2) spun forever inside
// the augmenting-path loop because all m=2 columns became matched after the
// first 2 rows, leaving row 3 with no free slot to augment into. The inner
// loop set used[j]=true for every j=1..m, found delta=inf and j1=0 on each
// pass, but p[0] equalled the live row index (not 0), so the break condition
// `p[j0] == 0` was never satisfied. The per-row ctx.Err() check was outside
// this loop, so even a cancelled context could not unblock it.
//
// After the fix:
//   - HungarianCtx returns ErrInvalidInput immediately for n > m.
//   - The inner loop checks ctx.Err() on every iteration, so a cancelled
//     context aborts promptly.
func TestHungarian_NGreaterThanM(t *testing.T) {
	t.Parallel()

	// Gate: 3x2 matrix must be rejected with ErrInvalidInput, not hang.
	// The test itself carries no explicit timeout; the -timeout flag on the
	// test binary (default 10 min) is the safety net. The repro hangs >5s,
	// so any fast return proves the fix is effective.
	cost := []float64{1, 2, 3, 4, 5, 6}
	_, err := Hungarian(cost, 3, 2)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Hungarian(3x2): expected ErrInvalidInput, got %v", err)
	}
}

// TestHungarian_NGreaterThanM_AlreadyCancelledCtx verifies that HungarianCtx
// with an already-cancelled context also returns immediately (inner-loop guard).
func TestHungarian_NGreaterThanM_AlreadyCancelledCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	// Use a valid n<=m matrix so the only early-return path is context
	// cancellation, exercising the inner-loop ctx.Err() check.
	cost := []float64{1, 2, 3, 4}
	_, err := HungarianCtx(ctx, cost, 2, 2)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestHungarian_NGreaterThanM_ValidCasesUnchanged verifies that the guard
// does not disturb correct n<=m results.
func TestHungarian_NGreaterThanM_ValidCasesUnchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cost     []float64
		n, m     int
		wantCost float64
		wantUniq bool // all assigned columns are distinct
	}{
		{
			name:     "1x1",
			cost:     []float64{7},
			n:        1,
			m:        1,
			wantCost: 7,
			wantUniq: true,
		},
		{
			name:     "2x2",
			cost:     []float64{4, 1, 2, 3},
			n:        2,
			m:        2,
			wantCost: 3, // row0→col1 (1) + row1→col0 (2) = 3
			wantUniq: true,
		},
		{
			name:     "2x3_rect",
			cost:     []float64{3, 0, 7, 2, 5, 0},
			n:        2,
			m:        3,
			wantCost: 0,
			wantUniq: true,
		},
		{
			name: "square_3x3",
			cost: []float64{
				4, 1, 3,
				2, 0, 5,
				3, 2, 2,
			},
			n:        3,
			m:        3,
			wantCost: 5,
			wantUniq: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := Hungarian(tc.cost, tc.n, tc.m)
			if err != nil {
				t.Fatalf("Hungarian(%s): unexpected error: %v", tc.name, err)
			}
			if tc.wantUniq {
				seen := map[int]bool{}
				for _, c := range a.RowToCol {
					if c < 0 {
						t.Fatalf("RowToCol contains -1: %v", a.RowToCol)
					}
					if seen[c] {
						t.Fatalf("duplicate column %d in %v", c, a.RowToCol)
					}
					seen[c] = true
				}
			}
			if a.TotalCost != tc.wantCost {
				t.Fatalf("TotalCost = %f, want %f", a.TotalCost, tc.wantCost)
			}
		})
	}
}
