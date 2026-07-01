package exec_test

// parallel_scan_project_budget_test.go — result-memory-budget bounding for the
// morsel-parallel fused scan (audit finding F3, #1830).
//
// Before the fix, ParallelScanProject accumulated the ENTIRE projected result
// set into per-worker buffers before Next handed the first row to the drain
// layer, so the engine's MaxResultRows / MaxResultBytes caps — enforced
// incrementally at the drain — could not bound peak memory on the parallel
// path. WithResultBudget threads the engine budget into the workers so they stop
// accumulating once the fleet-wide total exceeds it.
//
// These tests are deliberately NON-VACUOUS: without WithResultBudget the
// operator emits every matching row (10000 here), so asserting that a budgeted
// run emits only a bounded prefix (~budget + at most one in-flight batch per
// worker) proves the workers actually stop early rather than materialise the
// whole set. A row-count assertion alone at the drain layer would be vacuous —
// the drain trips the cap error either way; only the count of rows the operator
// materialises distinguishes the fix.

import (
	"context"
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestParallelScanProject_ResultBudgetBoundsRows(t *testing.T) {
	defer goleak.VerifyNone(t)

	const (
		n         = 20000 // 10000 even rows pass evenTimesTenFactory's filter
		maxRows   = 100
		allEven   = n / 2
		morselSz  = 64
		hardUpper = maxRows + 4096 // budget + a generous per-fleet overshoot ceiling
	)

	op := exec.NewParallelScanProject(buildWalker(n), evenTimesTenFactory, morselSz, nil).
		WithResultBudget(maxRows, 0, nil)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The operator must emit MORE than the budget (so the drain layer trips its
	// canonical ErrResultRowsExceeded) but FAR FEWER than the full result set (so
	// peak memory is bounded, not the whole set).
	if len(rows) <= maxRows {
		t.Fatalf("emitted %d rows; want > maxRows=%d so the drain cap trips", len(rows), maxRows)
	}
	if len(rows) >= allEven {
		t.Fatalf("emitted %d rows; the budget did not bound materialisation (full set is %d)", len(rows), allEven)
	}
	// Overshoot is bounded by roughly one in-flight row per worker.
	if len(rows) > hardUpper {
		t.Fatalf("emitted %d rows; overshoot beyond maxRows=%d exceeds the expected fleet bound", len(rows), maxRows)
	}
	t.Logf("bounded materialisation: %d rows (budget %d, %d workers, full set %d)",
		len(rows), maxRows, runtime.GOMAXPROCS(0), allEven)
}

func TestParallelScanProject_ResultBudgetBoundsBytes(t *testing.T) {
	defer goleak.VerifyNone(t)

	const (
		n            = 20000
		allEven      = n / 2
		bytesPerRow  = 100
		maxBytes     = 100 * bytesPerRow // ~100 rows' worth
		morselSz     = 64
		hardUpperRow = (maxBytes / bytesPerRow) + 4096
	)

	op := exec.NewParallelScanProject(buildWalker(n), evenTimesTenFactory, morselSz, nil).
		WithResultBudget(0, maxBytes, func(exec.Row) int64 { return bytesPerRow })
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) >= allEven {
		t.Fatalf("emitted %d rows; the byte budget did not bound materialisation (full set is %d)", len(rows), allEven)
	}
	if len(rows) > hardUpperRow {
		t.Fatalf("emitted %d rows; byte-budget overshoot exceeds the expected fleet bound", len(rows))
	}
}

// TestParallelScanProject_NoBudgetUnchanged pins that with no budget the operator
// still emits the full result set (the under-budget contract the differential
// multiset test relies on).
func TestParallelScanProject_NoBudgetUnchanged(t *testing.T) {
	defer goleak.VerifyNone(t)
	const n = 4000
	op := exec.NewParallelScanProject(buildWalker(n), evenTimesTenFactory, 64, nil).
		WithResultBudget(0, 0, nil)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != n/2 {
		t.Fatalf("emitted %d rows; want the full %d (no budget must not drop rows)", len(rows), n/2)
	}
}
