package cypher

// parallel_scan_project_budget_test.go — end-to-end result-cap enforcement on
// the morsel-parallel fused scan path (audit finding F3, #1830).
//
// The operator-level bounding (workers stop accumulating at the budget) is
// proven non-vacuously in cypher/exec/parallel_scan_project_budget_test.go.
// This test confirms the integration: a query that engages the parallel path
// over a graph above the threshold, run through an engine with a small
// MaxResultRows / MaxResultBytes, still surfaces the canonical
// ErrResultRowsExceeded / ErrResultBytesExceeded from the drain layer — i.e.
// bounding the materialisation did not defeat the cap error.

import (
	"context"
	"errors"
	"testing"
)

func runParallelCappedQuery(t *testing.T, opts *EngineOptions, q string) error {
	t.Helper()
	// 200 nodes > psTestThreshold so the fused parallel path engages.
	g := buildPSTestGraph(t, 200)
	opts.ParallelScanThreshold = psTestThreshold
	e := NewEngineWithOptions(g, *opts)

	before := parallelScanProjectBuildCount.Load()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // drain to trigger the cap check
	}
	drainErr := res.Err()
	_ = res.Close()
	if parallelScanProjectBuildCount.Load() == before {
		t.Fatalf("parallel fused scan did not engage for %q; test would be vacuous", q)
	}
	return drainErr
}

func TestParallelScanProject_ResultRowCapEnforced(t *testing.T) {
	// MaxResultRows=10 over 200 matching rows on the parallel path.
	err := runParallelCappedQuery(t, &EngineOptions{MaxResultRows: 10}, `MATCH (n) WHERE n.v >= 0 RETURN n.v AS v`)
	if !errors.Is(err, ErrResultRowsExceeded) {
		t.Fatalf("parallel-path drain error = %v; want ErrResultRowsExceeded", err)
	}
}

func TestParallelScanProject_ResultByteCapEnforced(t *testing.T) {
	// A tiny byte budget over 200 matching rows on the parallel path: the fused
	// workers stop accumulating and the drain trips the byte cap.
	err := runParallelCappedQuery(t, &EngineOptions{MaxResultBytes: 200}, `MATCH (n) WHERE n.v >= 0 RETURN n.v AS v`)
	if !errors.Is(err, ErrResultBytesExceeded) {
		t.Fatalf("parallel-path drain error = %v; want ErrResultBytesExceeded", err)
	}
}
