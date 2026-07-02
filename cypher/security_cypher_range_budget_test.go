package cypher_test

// security_cypher_range_budget_test.go — security engagement 2026-07-02 R2 (#1852).
//
// Finding D1 (CWE-770 / CWE-789): range() bypassed the per-evaluation
// list-element budget via its own 1e8 cap, so `RETURN range(1, 1e8)` allocated
// ~2.4 GB and a multi-column query `RETURN range(1,1e7), range(1,1e7), …`
// compounded to tens of GB in a single output row → OOM.
//
// The fix has two layers, both proven here:
//   1. evalFunction charges a function's returned list against the shared
//      per-evaluation budget, and maxRangeElements is lowered to 1e7 (==
//      expr.DefaultMaxListElements). This bounds ONE column expression.
//   2. exec.Project enforces an INCREMENTAL per-row byte budget (reusing the
//      engine's MaxResultBytes and the estimateValueSize deep-counter), so
//      several columns that each fit but whose SUM does not are rejected before
//      the whole row is materialised (ErrProjectionRowTooLarge).
//
// Both layers are TCK-neutral: the largest range()/function list in the entire
// openCypher TCK is 1,000,001 elements (range(1000000,2000000),
// Aggregation3.feature), five orders of magnitude of headroom below 1e7, and
// every conforming result already fits the default 1 GiB per-result budget.
// The positive control TestSec_Cypher_Range_LargestTCKRangeStillWorks pins that
// floor.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runTerminalErr runs q and returns the terminal error, whether it surfaces at
// Run() (plan errors) or during iteration via Result.Err() (evaluation errors).
// It also returns the number of rows drained. It never retains an unbounded
// payload: the per-row/per-call budgets fail-stop before a giant allocation.
func runTerminalErr(t *testing.T, eng *cypher.Engine, q string) (rows int, err error) {
	t.Helper()
	res, runErr := eng.Run(context.Background(), q, nil)
	if runErr != nil {
		return 0, runErr
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		rows++
	}
	return rows, res.Err()
}

// TestSec_Cypher_Range_SingleOversizedRejected pins layer 1: a single
// range(1, 1e8) is rejected by the lowered maxRangeElements cap with a typed
// *expr.EvalError, before any allocation — not decoded into a ~2.4 GB list.
func TestSec_Cypher_Range_SingleOversizedRejected(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	_, err := runTerminalErr(t, eng, "RETURN range(1, 100000000) AS r")
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("RETURN range(1,1e8) terminal err = %v, want a typed *expr.EvalError (range cap must reject before allocating)", err)
	}
}

// TestSec_Cypher_Range_MultiColumnRejectedButSingleFits pins layer 2: with a
// per-result byte budget sized so ONE range(1,100000) column fits (~1.6 MB,
// 16*(N+1)) but TWO do not (~3.2 MB), the single-column query completes while
// the two-column query is rejected by the incremental per-row guard with
// ErrProjectionRowTooLarge — proving multi-column compounding is caught even
// though each column is individually under budget.
func TestSec_Cypher_Range_MultiColumnRejectedButSingleFits(t *testing.T) {
	t.Parallel()
	// estimateValueSize(range(1,N)) = 16*(N+1). N=100000 -> ~1.6 MB per column.
	// Budget between one (1.6 MB) and two (3.2 MB) columns.
	const budget = 2_400_000
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: budget})

	// Positive: a single column fits under the budget and returns its row.
	rows, err := runTerminalErr(t, eng, "RETURN range(1, 100000) AS a")
	if err != nil {
		t.Fatalf("single range(1,100000) column under budget was rejected: %v", err)
	}
	if rows != 1 {
		t.Fatalf("single-column query produced %d rows, want 1", rows)
	}

	// Negative: two columns each fit but their sum exceeds the per-row budget,
	// so the incremental Project guard trips on the second column.
	_, err = runTerminalErr(t, eng, "RETURN range(1, 100000) AS a, range(1, 100000) AS b")
	if !errors.Is(err, exec.ErrProjectionRowTooLarge) {
		t.Fatalf("two-column range compounding terminal err = %v, want exec.ErrProjectionRowTooLarge "+
			"(per-row guard must reject the compounded row)", err)
	}
}

// TestSec_Cypher_Range_LargestTCKRangeStillWorks is the TCK floor: the largest
// range in the openCypher TCK — range(1000000, 2000000), 1,000,001 elements
// (Aggregation3.feature) — must still evaluate under the default engine. Both
// the 1e7 per-column budget and the 1 GiB per-row byte guard sit far above it,
// so this control fences the fix against over-aggressive capping.
func TestSec_Cypher_Range_LargestTCKRangeStillWorks(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t) // default 1 GiB result budget
	rows, err := runTerminalErr(t, eng, "RETURN size(range(1000000, 2000000)) AS n")
	if err != nil {
		t.Fatalf("largest TCK range range(1000000,2000000) was rejected: %v — the fix must stay above the TCK floor", err)
	}
	if rows != 1 {
		t.Fatalf("got %d rows, want 1", rows)
	}
}
