package csv_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestCSVRead_CtxCancelMidStream verifies that ReadIntoCtx honours
// context cancellation and deadline expiry, returning the appropriate
// sentinel error and a nil graph (the import is all-or-nothing at the
// in-memory level, so no partial graph escapes on cancellation).
//
// Goroutine-leak verification is handled by the package-level TestMain.
func TestCSVRead_CtxCancelMidStream(t *testing.T) {
	t.Parallel()

	t.Run("pre_cancelled", func(t *testing.T) {
		t.Parallel()
		// Cancel before calling ReadIntoCtx; the check at rows=0
		// (0&0xFFF==0) fires immediately.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		a, _, err := csv.ReadIntoCtx(ctx, strings.NewReader("a,b,1\n"), csv.DefaultOptions())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		// The graph must be nil on cancellation — no partial escapes.
		if a != nil {
			t.Errorf("graph = %v, want nil on cancellation", a)
		}
	})

	t.Run("deadline_exceeded_large_input", func(t *testing.T) {
		t.Parallel()
		// Build a reader large enough that the 4096-row batch boundary
		// is crossed, then let the timeout expire before all rows are
		// consumed.  ctx.Err() is polled every 4096 rows, so we need
		// at least one full batch; 200 000 rows is well past that.
		var sb strings.Builder
		sb.Grow(200_000 * 12)
		for i := range 200_000 {
			fmt.Fprintf(&sb, "a%d,b%d,1\n", i, i)
		}
		input := sb.String()

		// 1 µs is tight enough to expire before all 200 000 rows are
		// consumed but does not depend on scheduling jitter to fire
		// within the first batch.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Microsecond)
		defer cancel()

		// Sleep long enough that the deadline definitely expires before
		// we hand the reader to ReadIntoCtx.
		time.Sleep(100 * time.Microsecond)

		a, _, err := csv.ReadIntoCtx(ctx, strings.NewReader(input), csv.DefaultOptions())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want context.DeadlineExceeded, got %v", err)
		}
		// The graph must be nil on deadline expiry — no partial escapes.
		if a != nil {
			t.Errorf("graph = %v, want nil on deadline exceeded", a)
		}
	})
}
