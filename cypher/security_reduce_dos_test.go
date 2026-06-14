package cypher_test

// security_reduce_dos_test.go — REGRESSION GUARDS for two expression-level
// denial-of-service gaps that are now FIXED, plus a POSITIVE lock-in for the
// row/operator paths that have always been cancellable.
//
// ── SECURITY-GAP #1475 — per-expression list-size cap (FIXED) ────────────────
// reduce() / list comprehensions used to build their result list with no upper
// bound on the number of ELEMENTS the expression itself materialises. A doubling
// reduce
//   reduce(acc=[0], i IN range(1,N) | acc + acc)
// produced 2^N elements from a query whose text is O(1); nested comprehensions
// multiplied element counts as N^depth. The Engine's MaxResultRows /
// MaxResultBytes caps bound only the number of RESULT ROWS and their serialised
// size, and funcs.DefaultMaxCollectItems bounds collect()/percentile aggregators
// — none bounded a single intermediate list grown inside ONE expression
// evaluation. The fix adds a per-evaluation cumulative element budget
// ([expr.DefaultMaxListElements] = 10,000,000); an expression that exceeds it
// returns a typed [expr.EvalError] (fail-stop) rather than allocating without
// bound. The tests below drive each variant PAST the budget and assert the
// typed error.
//
// ── SECURITY-GAP #1477 — reduce/comprehension loop honours context (FIXED) ────
// The list-iteration helpers (reduce, comprehension, quantifier) used to iterate
// with a plain `for _, elem := range list` and no context check, so a deadline
// or cancellation that fired WHILE the loop ran was observed only after the loop
// finished. The fix polls the context every 4096 iterations (the executor's
// convention) and returns the context error promptly. The test below sets a
// deadline at a small fraction of the warm runtime and asserts a prompt abort
// with context.DeadlineExceeded and a value that was NOT fully computed.
//
// The positive lock-in (TestSec_Cypher_ScanIsCancellable) fences the
// operator-level cancellation contract against regression.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestSec_Cypher_Reduce_DoublingRejectedByBudget locks in the fix for
// SECURITY-GAP #1475. A doubling reduce at exponent 30 would, unbounded,
// materialise 2^30 ≈ 1.07e9 IntegerValue elements (gigabytes). The
// per-evaluation list-element budget ([expr.DefaultMaxListElements] = 1e7) trips
// long before that — at the step where the accumulator concat would exceed the
// budget — and the query fails fast with a typed [expr.EvalError] instead of
// allocating. The exponent is well past the cap, so the budget MUST fire; the
// allocation never approaches the budget's worst case because the charge is
// applied before each concat's make().
func TestSec_Cypher_Reduce_DoublingRejectedByBudget(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const exp = 30 // 2^30 ≈ 1.07e9 elements >> the 1e7 budget; the cap MUST trip.
	q := "RETURN size(reduce(acc=[0], i IN range(1," + strconv.Itoa(exp) + ") | acc + acc)) AS s"

	secCypherAssertListBudgetError(t, eng, q)
}

// TestSec_Cypher_NestedComprehension_RejectedByBudget locks in the fix for the
// multiplicative variant of #1475: three nested list comprehensions build N^3
// inner elements from O(1) query text. At N=300 the innermost work is
// 300^3 = 2.7e7 elements, which exceeds the per-evaluation budget
// ([expr.DefaultMaxListElements] = 1e7). Because the budget is CUMULATIVE across
// the whole expression evaluation (not per-list), the nested structure — whose
// individual lists are each only N=300 elements — still trips the cap once the
// running total of materialised elements crosses 1e7, returning a typed
// [expr.EvalError]. This proves the budget catches N^depth multiplication, not
// just a single oversized list.
func TestSec_Cypher_NestedComprehension_RejectedByBudget(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const n = 300 // 300^3 = 2.7e7 cumulative inner elements > the 1e7 budget.
	ns := strconv.Itoa(n)
	q := "RETURN size([a IN range(1," + ns + ") | [b IN range(1," + ns + ") | [c IN range(1," + ns + ") | a+b+c]]]) AS s"

	secCypherAssertListBudgetError(t, eng, q)
}

// TestSec_Cypher_Reduce_LoopHonoursDeadline locks in the fix for
// SECURITY-GAP #1477. A reduce over a moderate list is timed warm, then re-run
// with a deadline set to a small fraction of that warm duration so the deadline
// fires WHILE the loop is running. The loop now polls the context every 4096
// iterations, so the call aborts PROMPTLY: the elapsed wall-time is far below
// warm, Result.Err() is context.DeadlineExceeded, and the reduce value is NOT
// fully computed (the loop stopped mid-iteration).
//
// The element count (500_000) is bounded: ~tens of ms of work, negligible
// memory, safe under -race and within the package budget.
func TestSec_Cypher_Reduce_LoopHonoursDeadline(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const n = 500_000                 // ~tens of ms of integer additions.
	want := int64(n) * int64(n+1) / 2 // sum 1..n (the full, uninterrupted result)
	q := "RETURN reduce(acc=0, i IN range(1," + strconv.Itoa(n) + ") | acc + i) AS r"

	// 1) Warm run with no deadline to measure the uninterrupted loop duration
	//    and confirm the full sum is produced when the context never fires.
	warm, fullVal, fullErr := secCypherReduceSumRun(context.Background(), t, eng, q)
	if fullErr != nil {
		t.Fatalf("warm Run(%q): unexpected error %v", q, fullErr)
	}
	if fullVal != want {
		t.Fatalf("warm Run(%q): r=%d; want %d (uninterrupted reduce must compute the full sum)", q, fullVal, want)
	}
	if warm < 2*time.Millisecond {
		// Implausibly fast — the timing comparison would be meaningless. The
		// cancellation contract is still asserted by the error/value checks
		// below, just without the wall-time bound.
		t.Logf("warm reduce ran in %v (very fast machine); asserting cancellation by error/value only", warm)
	}

	// 2) Deadline at ~1/20th of warm: it is certain to elapse mid-loop. The loop
	//    now honours it and aborts promptly.
	deadline := warm / 20
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	elapsed, got, iterErr := secCypherReduceSumRun(ctx, t, eng, q)

	// The loop was cut short: a context.DeadlineExceeded surfaced…
	if !errors.Is(iterErr, context.DeadlineExceeded) {
		t.Fatalf("SECURITY-GAP #1477 NOT enforced: reduce under a %v deadline (warm=%v) returned err=%v; want context.DeadlineExceeded — the in-expression loop is not honouring the context mid-iteration", deadline, warm, iterErr)
	}
	// …and the value was NOT fully computed (the loop stopped before the end).
	if got == want {
		t.Fatalf("SECURITY-GAP #1477 NOT enforced: reduce under a %v deadline still produced the full sum %d — the loop ran to completion instead of aborting", deadline, want)
	}
	// …and (when the timing is meaningful) the abort was prompt, not ~warm.
	if warm >= 2*time.Millisecond && elapsed > warm/2 {
		t.Fatalf("SECURITY-GAP #1477 weak: reduce took %v under a %v deadline (warm=%v); a prompt abort should be far below warm/2 — the context check stride may be too coarse", elapsed, deadline, warm)
	}
	t.Logf("SECURITY-GAP #1477 fixed: reduce aborted in %v (warm %v) under a %v deadline with context.DeadlineExceeded; partial value %d (full would be %d)", elapsed, warm, deadline, got, want)
}

// secCypherReduceSumRun runs q (RETURN one integer column "r") under ctx and
// returns the elapsed wall-time, the produced value (0 when no row carried "r"),
// and the iteration error (nil on clean completion). It fails the test only on
// an unexpected Run-entry error; the caller asserts the iteration outcome.
func secCypherReduceSumRun(ctx context.Context, t *testing.T, eng *cypher.Engine, q string) (time.Duration, int64, error) {
	t.Helper()
	t0 := time.Now()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		// A pre-iteration deadline rejection is a legitimate prompt abort.
		if errors.Is(err, context.DeadlineExceeded) {
			return time.Since(t0), 0, err
		}
		t.Fatalf("Run(%q): unexpected entry error %v", q, err)
	}
	var got int64
	for res.Next() {
		if iv, ok := res.Record()["r"].(expr.IntegerValue); ok {
			got = int64(iv)
		}
	}
	iterErr := res.Err()
	elapsed := time.Since(t0)
	_ = res.Close()
	return elapsed, got, iterErr
}

// secCypherAssertListBudgetError runs q and asserts the per-evaluation
// list-element budget (#1475) trips with a typed [expr.EvalError]. The error may
// surface at Run entry or during iteration depending on when the engine
// materialises the projection; both are accepted. The query MUST NOT complete
// successfully — an unbounded list would otherwise pin memory.
func secCypherAssertListBudgetError(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		secCypherRequireEvalError(t, q, err)
		return
	}
	for res.Next() { //nolint:revive // drain to reach the materialisation error
	}
	iterErr := res.Err()
	_ = res.Close()
	if iterErr == nil {
		t.Fatalf("Run(%q): completed without error — the per-evaluation list-element budget (#1475) did not trip; an unbounded list was materialised", q)
	}
	secCypherRequireEvalError(t, q, iterErr)
}

// secCypherRequireEvalError fails the test unless err is (or wraps) an
// [expr.EvalError]. Used by the #1475 budget guards to confirm the failure is
// the typed eval-limit error and not some unrelated failure.
func secCypherRequireEvalError(t *testing.T, q string, err error) {
	t.Helper()
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("Run(%q): error %v (%T) is not an *expr.EvalError — the list-element budget must fail-stop with the typed eval error", q, err, err)
	}
	t.Logf("SECURITY-GAP #1475 fixed: %q rejected with typed *expr.EvalError: %v", q, ee)
}

// TestSec_Cypher_ScanIsCancellable is the POSITIVE lock-in counterpart to #1477:
// the row/operator-level paths DO honour context today. A large UNWIND scan
// under an already-cancelled context must NOT run unbounded — Run rejects at
// entry, or the iteration stops with a context error. This proves the gap is
// specific to in-expression loops, not the engine's cancellation contract as a
// whole, and fences the cancellable behaviour against regression.
func TestSec_Cypher_ScanIsCancellable(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run

	// A large UNWIND that, if the scan ignored ctx, would emit ~10M rows.
	res, err := eng.Run(ctx, `UNWIND range(0, 9999999) AS x RETURN x`, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run rejected with a non-context error: %v", err)
		}
		return // rejected at entry — correct, scan never ran
	}
	defer func() { _ = res.Close() }()

	rows := 0
	for res.Next() {
		rows++
		if rows > 1_000_000 {
			t.Fatalf("scan emitted >1M rows under a cancelled context — the operator path ignored cancellation (this would be a NEW regression, distinct from the #1477 expression-loop gap)")
		}
	}
	if iterErr := res.Err(); iterErr != nil && !errors.Is(iterErr, context.Canceled) && !errors.Is(iterErr, context.DeadlineExceeded) {
		t.Fatalf("scan iteration ended with an unexpected error: %v", iterErr)
	}
}
