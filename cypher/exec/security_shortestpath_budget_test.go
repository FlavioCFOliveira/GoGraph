package exec_test

// security_shortestpath_budget_test.go — REGRESSION GUARD for the 2026-07-02
// audit finding (#1840): the EXHAUSTIVE shortestPath / allShortestPaths search
// (entered when the pattern carries a whole-path WHERE predicate) had no
// edge-traversal or hop work budget — only ctx cancellation. A hostile
// unsatisfiable path predicate over an unbounded pattern on a dense graph could
// therefore drive super-exponential frontier growth whose PEAK MEMORY is bounded
// only by a wall-clock deadline, not by any explicit resource limit — the same
// DoS/OOM class VarLengthExpand already defends against (#1478).
//
// The fix gives the exhaustive search the identical VarLengthExpand budget: a
// per-input-row edge cap, an aggregate per-query edge cap that is NOT reset per
// row, and a finite hop ceiling when the pattern is unbounded. These white-box
// tests inject small caps (via WithWorkBudget) so the per-row and aggregate
// semantics are proven deterministically without performing millions of real
// traversals; a control with a generous budget proves a legitimate exhaustive
// search that finds a path is never rejected.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// completeDigraph builds a complete directed graph on n nodes (every i→j, i≠j),
// dense enough that an unsatisfiable-predicate exhaustive search enumerates many
// edge-simple paths. rev is empty (the DirOut exhaustive search uses forward
// adjacency only). Per-edge handles are the CSR positions (staticCSR leaves
// handles nil, and the operator falls back to the unique forward position), so
// relationship-uniqueness distinguishes every edge.
func completeDigraph(n int) (fwd, rev *staticCSR) {
	var edges [][2]int
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				edges = append(edges, [2]int{i, j})
			}
		}
	}
	return buildCSR(n, edges), buildCSR(n, nil)
}

// alwaysFalsePred is an unsatisfiable whole-path predicate: the exhaustive search
// can never accept, so it enumerates until a resource bound stops it.
func alwaysFalsePred(exec.Row) (bool, error) { return false, nil }

// alwaysTruePred accepts the first (shortest) candidate.
func alwaysTruePred(exec.Row) (bool, error) { return true, nil }

func srcDstRow(src, dst int) exec.Row {
	return exec.Row{expr.IntegerValue(int64(src)), expr.IntegerValue(int64(dst))}
}

// TestSec_ShortestPath_ExhaustiveBudget_PerRowTrips locks in the per-input-row
// edge cap. On a complete digraph with an unsatisfiable predicate and a per-row
// cap of 4, the frontier's edge traversals cross 4 within the first two BFS
// levels, so the operator returns ErrVarLenCapExceeded instead of enumerating an
// exponential number of paths.
func TestSec_ShortestPath_ExhaustiveBudget_PerRowTrips(t *testing.T) {
	t.Parallel()
	fwd, rev := completeDigraph(5)
	input := newSliceOperator(srcDstRow(0, 4))
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1).
		WithPathPredicate(alwaysFalsePred).
		WithWorkBudget(4, 0) // per-row cap 4; aggregate default (huge)

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("exhaustive shortestPath with an unsatisfiable predicate under a per-row edge cap of 4 returned err=%v; want ErrVarLenCapExceeded — the work budget (#1840) is not enforced", err)
	}
}

// TestSec_ShortestPath_ExhaustiveBudget_AggregateTripsAcrossRows locks in the
// aggregate per-query cap. Each of the 20 input rows expands only a handful of
// edges before the (large) per-row cap would fire, but the aggregate cap of 10
// is crossed once the running total accumulates across rows — which can only
// happen if the counter is NOT reset per input row.
func TestSec_ShortestPath_ExhaustiveBudget_AggregateTripsAcrossRows(t *testing.T) {
	t.Parallel()
	fwd, rev := completeDigraph(5)
	rows := make([]exec.Row, 20)
	for i := range rows {
		rows[i] = srcDstRow(0, 4)
	}
	input := newSliceOperator(rows...)
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1).
		WithPathPredicate(alwaysFalsePred).
		WithWorkBudget(1_000_000, 10) // per-row never fires; aggregate cap 10

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("aggregate exhaustive-search edge budget of 10 across 20 rows returned err=%v; want ErrVarLenCapExceeded — the aggregate counter must accumulate across input rows, not reset per row", err)
	}
}

// TestSec_ShortestPath_ExhaustiveBudget_GenerousCapCompletes is the control: a
// legitimate exhaustive search that finds a satisfying path is never rejected by
// the caps. With a generous budget and an accepting predicate, the operator
// returns the shortest path (the direct 0→4 edge) and no error.
func TestSec_ShortestPath_ExhaustiveBudget_GenerousCapCompletes(t *testing.T) {
	t.Parallel()
	fwd, rev := completeDigraph(5)
	input := newSliceOperator(srcDstRow(0, 4))
	op := exec.NewShortestPath(input, fwd, rev, exec.DirOut, 0, 1).
		WithPathPredicate(alwaysTruePred).
		WithWorkBudget(1_000_000, 1_000_000)

	got, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v — a generous work budget must not reject a legitimate exhaustive search", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows; want 1 (the shortest satisfying path 0→4)", len(got))
	}
}

// TestSec_AllShortestPaths_ExhaustiveBudget_PerRowTrips mirrors the per-row cap
// guard for the allShortestPaths exhaustive search.
func TestSec_AllShortestPaths_ExhaustiveBudget_PerRowTrips(t *testing.T) {
	t.Parallel()
	fwd, rev := completeDigraph(5)
	input := newSliceOperator(srcDstRow(0, 4))
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1).
		WithPathPredicate(alwaysFalsePred).
		WithWorkBudget(4, 0)

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("exhaustive allShortestPaths with an unsatisfiable predicate under a per-row edge cap of 4 returned err=%v; want ErrVarLenCapExceeded", err)
	}
}

// TestSec_AllShortestPaths_ExhaustiveBudget_GenerousCapCompletes is the
// allShortestPaths control: a generous budget with an accepting predicate
// returns the shortest satisfying paths and no error.
func TestSec_AllShortestPaths_ExhaustiveBudget_GenerousCapCompletes(t *testing.T) {
	t.Parallel()
	fwd, rev := completeDigraph(5)
	input := newSliceOperator(srcDstRow(0, 4))
	op := exec.NewAllShortestPaths(input, fwd, rev, exec.DirOut, 0, 1).
		WithPathPredicate(alwaysTruePred).
		WithWorkBudget(1_000_000, 1_000_000)

	got, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v — a generous work budget must not reject a legitimate exhaustive search", err)
	}
	// The shortest length from 0 to 4 in a complete digraph is 1; every direct
	// edge of that length is a single path (0→4), so exactly one row is emitted.
	if len(got) != 1 {
		t.Fatalf("got %d rows; want 1 (the single shortest satisfying path 0→4)", len(got))
	}
}
