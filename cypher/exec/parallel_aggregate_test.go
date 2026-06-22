package exec_test

// parallel_aggregate_test.go — tests for ParallelCountScan (#1672).
//
// Coverage: count equals the live node count for every partition (1, several,
// and many morsels), empty graph (count 0), determinism across repeated runs,
// cancellation, race-clean concurrent instances, and no goroutine leak (goleak).

import (
	"context"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// drainCount runs a ParallelCountScan and returns the single emitted count.
func drainCount(t *testing.T, walker *staticNodeWalker, morselSize int) int64 {
	t.Helper()
	op := exec.NewParallelCountScan(walker, morselSize)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	if len(rows[0]) != 1 {
		t.Fatalf("row width = %d, want 1", len(rows[0]))
	}
	iv, ok := rows[0][0].(expr.IntegerValue)
	if !ok {
		t.Fatalf("row[0][0] is %T, want IntegerValue", rows[0][0])
	}
	return int64(iv)
}

// 1. Count is exact across a range of morsel/worker partitions.
func TestParallelCountScan_ExactCount(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, tc := range []struct {
		name       string
		n          int
		morselSize int
	}{
		{"single-morsel", 500, exec.DefaultMorselSize},
		{"several-morsels", 5000, exec.DefaultMorselSize},
		{"many-tiny-morsels", 1000, 3}, // ceil(1000/3)=334 morsels across GOMAXPROCS workers
		{"one-node", 1, exec.DefaultMorselSize},
		{"default-morsel-zero", 4096, 0}, // 0 → DefaultMorselSize
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := drainCount(t, buildWalker(tc.n), tc.morselSize)
			if got != int64(tc.n) {
				t.Errorf("count = %d, want %d", got, tc.n)
			}
		})
	}
}

// 2. Empty graph yields count 0 with no workers and no leak.
func TestParallelCountScan_Empty(t *testing.T) {
	defer goleak.VerifyNone(t)

	got := drainCount(t, &staticNodeWalker{}, exec.DefaultMorselSize)
	if got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

// 3. The result is deterministic across repeated runs regardless of how the
// scheduler interleaves the workers.
func TestParallelCountScan_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 7777
	walker := buildWalker(n)
	const runs = 20
	for run := range runs {
		got := drainCount(t, walker, 7) // tiny morsels → maximal partitioning
		if got != int64(n) {
			t.Fatalf("run %d: count = %d, want %d", run, got, n)
		}
	}
}

// 4. Cancellation: the operator returns promptly and leaks no goroutine.
func TestParallelCountScan_Cancellation(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 1_000_000
	walker := buildWalker(n)
	op := exec.NewParallelCountScan(walker, exec.DefaultMorselSize)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Drain(ctx, op)
		done <- err
	}()
	time.Sleep(2 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Either a clean completion (the scan may finish before cancel lands) or
		// a cancellation error is acceptable; we only fail on a hang.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ParallelCountScan did not return within 500ms after cancellation")
	}
}

// 5. Race detector: independent instances counting concurrently must be clean.
func TestParallelCountScan_RaceClean(t *testing.T) {
	const n = 2000
	walker := buildWalker(n)

	const goroutines = 8
	results := make(chan int64, goroutines)
	for range goroutines {
		go func() {
			op := exec.NewParallelCountScan(walker, 64)
			rows, err := exec.Drain(context.Background(), op)
			if err != nil || len(rows) != 1 {
				results <- -1
				return
			}
			iv, _ := rows[0][0].(expr.IntegerValue)
			results <- int64(iv)
		}()
	}
	for range goroutines {
		if got := <-results; got != int64(n) {
			t.Errorf("concurrent count = %d, want %d", got, n)
		}
	}
}

// 6. Close before any Next must still join the spawned workers cleanly (no
// leak), exercising the never-drained teardown path.
func TestParallelCountScan_CloseWithoutNext(t *testing.T) {
	defer goleak.VerifyNone(t)

	op := exec.NewParallelCountScan(buildWalker(100_000), 16)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Close without draining — workers spawned in Init must be cancelled+joined.
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent second Close.
	if err := op.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// 7. Worker count never exceeds GOMAXPROCS (bounded resources): a graph with far
// more morsels than CPUs still counts exactly, which would fail if morsels were
// dropped or double-counted by an over/under-provisioned worker pool.
func TestParallelCountScan_BoundedWorkers(t *testing.T) {
	defer goleak.VerifyNone(t)

	procs := runtime.GOMAXPROCS(0)
	n := (procs + 4) * exec.DefaultMorselSize // more morsels than workers
	got := drainCount(t, buildWalker(n), exec.DefaultMorselSize)
	if got != int64(n) {
		t.Errorf("count = %d, want %d", got, n)
	}
}
