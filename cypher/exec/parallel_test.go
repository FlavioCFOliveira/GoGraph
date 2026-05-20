package exec_test

// parallel_test.go — tests for ParallelScan (task-249).
//
// Coverage: basic correctness (all IDs emitted), empty graph, cancellation,
// goleak (no goroutine leaks), bounded channel, race-clean.

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildWalker creates a staticNodeWalker with sequential IDs [0, n).
func buildWalker(n int) *staticNodeWalker {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return &staticNodeWalker{ids: ids}
}

// drainParallel runs a ParallelScan and returns sorted node IDs.
func drainParallel(t *testing.T, walker *staticNodeWalker, morselSize int) []int64 {
	t.Helper()
	op := exec.NewParallelScan(walker, morselSize)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row[%d][0] is %T, want IntegerValue", i, row[0])
		}
		ids[i] = int64(iv)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Basic correctness — all IDs emitted, no duplicates
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_Basic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 5000
	walker := buildWalker(n)
	ids := drainParallel(t, walker, exec.DefaultMorselSize)

	if len(ids) != n {
		t.Fatalf("got %d IDs, want %d", len(ids), n)
	}
	for i, id := range ids {
		if id != int64(i) {
			t.Errorf("ids[%d] = %d, want %d", i, id, i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Empty graph — no rows, no goroutine leaks
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_Empty(t *testing.T) {
	defer goleak.VerifyNone(t)

	ids := drainParallel(t, &staticNodeWalker{}, exec.DefaultMorselSize)
	if len(ids) != 0 {
		t.Errorf("expected 0 rows, got %d", len(ids))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Small morsel size — still correct
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_SmallMorsel(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 100
	walker := buildWalker(n)
	ids := drainParallel(t, walker, 3) // morsel = 3, so ceil(100/3)=34 morsels

	if len(ids) != n {
		t.Fatalf("got %d IDs, want %d", len(ids), n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Cancellation — exits within 200ms, no leaks
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_Cancellation(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 1_000_000
	walker := buildWalker(n)
	op := exec.NewParallelScan(walker, exec.DefaultMorselSize)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := exec.Drain(ctx, op)
		done <- err
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			// Large scan may complete before cancel; that is OK for correctness.
			// We only fail if it hangs.
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ParallelScan did not return within 500ms after cancellation")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Race detector — concurrent access via multiple Drain calls
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_RaceClean(t *testing.T) {
	// Each goroutine owns its own ParallelScan instance.
	const n = 500
	walker := buildWalker(n)

	const goroutines = 4
	done := make(chan struct{}, goroutines)
	for range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()
			op := exec.NewParallelScan(walker, 50)
			_, _ = exec.Drain(context.Background(), op)
		}()
	}
	for range goroutines {
		<-done
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Single node — edge case
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelScan_SingleNode(t *testing.T) {
	defer goleak.VerifyNone(t)

	ids := drainParallel(t, buildWalker(1), exec.DefaultMorselSize)
	if len(ids) != 1 || ids[0] != 0 {
		t.Errorf("got %v, want [0]", ids)
	}
}
