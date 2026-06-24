package exec_test

// parallel_scan_project_test.go — operator-level tests for ParallelScanProject
// (#1682).
//
// These exercise the operator in isolation with a stub SubplanFactory that builds
// AllNodesScan(morsel)→Filter→Project, independent of the cypher planner. Coverage:
// result multiset equals the serial subplan across partitions, deep-copy isolation
// (rows survive Project's reused outBuf), empty input, determinism, cancellation,
// a sub-plan build/exec error surfaced through Next, Close-without-Next teardown,
// bounded worker count, and no goroutine leak (goleak).

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// evenTimesTenFactory builds, for a morsel, an AllNodesScan over that morsel,
// a Filter that keeps even IDs, and a Project that maps each kept id to id*10.
// Each call returns a fresh, independent operator tree (the contract a real
// per-worker factory must honour).
func evenTimesTenFactory(morsel []graph.NodeID) (exec.Operator, error) {
	scan := exec.NewAllNodesScan(&staticNodeWalker{ids: morsel})
	filt := exec.NewFilter(scan, func(row exec.Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		return expr.BoolValue(int64(iv)%2 == 0), nil
	})
	return exec.NewProject(filt, []exec.ProjectionItem{{
		Alias: "x",
		Eval: func(row exec.Row) (expr.Value, error) {
			iv, _ := row[0].(expr.IntegerValue)
			return expr.IntegerValue(int64(iv) * 10), nil
		},
	}})
}

// serialEvenTimesTen computes the expected multiset of x = id*10 for even ids,
// directly from the walker's ids — the oracle the parallel result must equal.
func serialEvenTimesTen(ids []graph.NodeID) []int64 {
	var out []int64
	for _, id := range ids {
		if int64(id)%2 == 0 {
			out = append(out, int64(id)*10)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// drainProject runs a ParallelScanProject and returns the single-column int64
// values it emits, sorted (a full scan is unordered).
func drainProject(t *testing.T, walker *staticNodeWalker, factory exec.SubplanFactory, morselSize int) []int64 {
	t.Helper()
	op := exec.NewParallelScanProject(walker, factory, morselSize, nil)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := make([]int64, 0, len(rows))
	for _, r := range rows {
		if len(r) != 1 {
			t.Fatalf("row width = %d, want 1", len(r))
		}
		iv, ok := r[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row value is %T, want IntegerValue", r[0])
		}
		got = append(got, int64(iv))
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 1. The fused result multiset equals the serial oracle across partitions.
func TestParallelScanProject_MultisetMatchesSerial(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, tc := range []struct {
		name       string
		n          int
		morselSize int
	}{
		{"single-morsel", 500, exec.DefaultMorselSize},
		{"several-morsels", 5000, exec.DefaultMorselSize},
		{"many-tiny-morsels", 1000, 3},
		{"one-node-even", 2, exec.DefaultMorselSize},
		{"default-morsel-zero", 4096, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walker := buildWalker(tc.n)
			got := drainProject(t, walker, evenTimesTenFactory, tc.morselSize)
			want := serialEvenTimesTen(walker.ids)
			if !equalInt64(got, want) {
				t.Errorf("multiset mismatch: got %d values, want %d", len(got), len(want))
			}
		})
	}
}

// 2. Deep-copy isolation: Project reuses one outBuf, so without a per-row copy the
// buffered rows would all collapse to the last value. With many tiny morsels and
// distinct values, every value must survive distinct.
func TestParallelScanProject_DeepCopyIsolation(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 1000
	walker := buildWalker(n)
	got := drainProject(t, walker, evenTimesTenFactory, 1) // morsel of 1 → maximal reuse pressure
	want := serialEvenTimesTen(walker.ids)
	if !equalInt64(got, want) {
		t.Fatalf("deep-copy isolation failed: got %d distinct-ish values, want %d", len(got), len(want))
	}
	// Spot-check uniqueness: every value distinct (ids are distinct, *10 stays distinct).
	seen := map[int64]struct{}{}
	for _, v := range got {
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate value %d — rows were not deep-copied (outBuf aliasing)", v)
		}
		seen[v] = struct{}{}
	}
}

// 3. Empty input yields zero rows, no workers, no leak.
func TestParallelScanProject_Empty(t *testing.T) {
	defer goleak.VerifyNone(t)

	got := drainProject(t, &staticNodeWalker{}, evenTimesTenFactory, exec.DefaultMorselSize)
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}

// 4. Determinism across repeated runs despite worker interleaving.
func TestParallelScanProject_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 7777
	walker := buildWalker(n)
	want := serialEvenTimesTen(walker.ids)
	for run := range 20 {
		got := drainProject(t, walker, evenTimesTenFactory, 7)
		if !equalInt64(got, want) {
			t.Fatalf("run %d: multiset mismatch", run)
		}
	}
}

// 5. Cancellation returns promptly and leaks no goroutine.
func TestParallelScanProject_Cancellation(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 1_000_000
	walker := buildWalker(n)
	op := exec.NewParallelScanProject(walker, evenTimesTenFactory, exec.DefaultMorselSize, nil)

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
		// Clean completion or cancellation error both acceptable; only a hang fails.
	case <-time.After(2 * time.Second):
		t.Fatal("ParallelScanProject did not return within 2s after cancellation")
	}
}

// 6. A sub-plan error is surfaced through Next and leaks no goroutine.
func TestParallelScanProject_SubplanError(t *testing.T) {
	defer goleak.VerifyNone(t)

	sentinel := errors.New("boom")
	failFactory := func(morsel []graph.NodeID) (exec.Operator, error) {
		scan := exec.NewAllNodesScan(&staticNodeWalker{ids: morsel})
		// Filter whose predicate always errors, so draining the sub-plan fails.
		filt := exec.NewFilter(scan, func(_ exec.Row) (expr.Value, error) {
			return expr.Null, sentinel
		})
		return exec.NewProject(filt, []exec.ProjectionItem{{
			Alias: "x",
			Eval:  func(row exec.Row) (expr.Value, error) { return row[0], nil },
		}})
	}

	op := exec.NewParallelScanProject(buildWalker(5000), failFactory, 64, nil)
	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Drain error = %v, want it to wrap %v", err, sentinel)
	}
}

// 7. A factory build error is surfaced through Next and leaks no goroutine.
func TestParallelScanProject_FactoryError(t *testing.T) {
	defer goleak.VerifyNone(t)

	sentinel := errors.New("factory failed")
	op := exec.NewParallelScanProject(buildWalker(5000), func(_ []graph.NodeID) (exec.Operator, error) {
		return nil, sentinel
	}, 64, nil)
	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Drain error = %v, want it to wrap %v", err, sentinel)
	}
}

// 8. Close before any Next must cancel and join the spawned workers cleanly.
func TestParallelScanProject_CloseWithoutNext(t *testing.T) {
	defer goleak.VerifyNone(t)

	op := exec.NewParallelScanProject(buildWalker(100_000), evenTimesTenFactory, 16, nil)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// 9. Worker count is bounded by GOMAXPROCS: far more morsels than CPUs still
// produces the exact multiset.
func TestParallelScanProject_BoundedWorkers(t *testing.T) {
	defer goleak.VerifyNone(t)

	procs := runtime.GOMAXPROCS(0)
	n := (procs + 4) * exec.DefaultMorselSize
	walker := buildWalker(n)
	got := drainProject(t, walker, evenTimesTenFactory, exec.DefaultMorselSize)
	want := serialEvenTimesTen(walker.ids)
	if !equalInt64(got, want) {
		t.Errorf("multiset mismatch with %d morsels over %d workers", n/exec.DefaultMorselSize, procs)
	}
}

// 10. Independent instances running concurrently are race-clean.
func TestParallelScanProject_RaceClean(t *testing.T) {
	const n = 2000
	walker := buildWalker(n)
	want := serialEvenTimesTen(walker.ids)

	const goroutines = 8
	results := make(chan bool, goroutines)
	for range goroutines {
		go func() {
			op := exec.NewParallelScanProject(walker, evenTimesTenFactory, 64, nil)
			rows, err := exec.Drain(context.Background(), op)
			if err != nil {
				results <- false
				return
			}
			got := make([]int64, 0, len(rows))
			for _, r := range rows {
				iv, _ := r[0].(expr.IntegerValue)
				got = append(got, int64(iv))
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			results <- equalInt64(got, want)
		}()
	}
	for range goroutines {
		if !<-results {
			t.Error("concurrent ParallelScanProject produced wrong multiset")
		}
	}
}
