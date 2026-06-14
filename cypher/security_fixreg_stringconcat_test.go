package cypher_test

// security_fixreg_stringconcat_test.go — FIX-REGRESSION audit (SEC-2026-06-14b)
// of the per-evaluation list-element budget fix (#1475, commit a565e50).
//
// CONFIRMED NEW DEFECT — STRING-concatenation is NOT charged against the budget.
//
// The #1475 fix added a per-evaluation cumulative budget that charges every
// LIST element an expression materialises (cypher/expr/eval.go: the three
// chargeListGrowth calls in evalArith's list-concat branches, plus the per-row
// charge in evalListComprehension). It bounds the canonical doubling attack
//   reduce(acc=[0], i IN range(1,N) | acc + acc)        -- list accumulator
// because acc is a ListValue and each concat is charged.
//
// But evalArith's STRING-concatenation branch (eval.go ~line 972,
//   if ls, lok := left.(StringValue); lok { if rs, rok := right.(StringValue) ...
//     return StringValue(string(ls)+string(rs)), nil }
// ) returns BEFORE reaching any chargeListGrowth call and is never charged. The
// budget counts list ELEMENTS, not string BYTES. The exact same doubling attack
// with a STRING accumulator
//   reduce(s='AAAAAAAA', i IN range(1,N) | s + s)        -- string accumulator
// doubles the string to seedLen·2^N bytes from an O(1) query text, with NO
// EvalError — re-opening the very memory-exhaustion class #1475 set out to
// close (CWE-789 / CWE-400). The reduce loop's ctx check (#1477) fires only
// between iterations, so the deadline cannot abort the single giant concat
// either.
//
// These tests are BOUNDED: the positive (defect) test uses a small exponent so
// the produced string is a few MiB — enough to PROVE no EvalError fires and the
// growth is exponential and uncharged, without exhausting the host. The
// negative (lock-in) test re-asserts that the LIST accumulator is still caught,
// so a future fix that adds string charging must keep the list path working.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestSec_Cypher_StringConcat_DoublingBypassesBudget asserts the #1482 FIX: a
// string-accumulator reduce that grows past the per-evaluation byte budget now
// fails fast with a typed *expr.EvalError, where before #1482 it materialised
// seedLen·2^N bytes uncharged. The over-budget probe uses the O(N²)
// linear-append form (s + 'a' per iteration): the CUMULATIVE byte charge ≈K²/2
// crosses the 1 GiB budget (expr.DefaultMaxStringEvalBytes) at K≈46341 while the
// PEAK string stays ≈46 KiB, so the budget fires cheaply and host-safely.
func TestSec_Cypher_StringConcat_DoublingBypassesBudget(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const k = 50000 // Σ(1+i) ≈ K²/2 ≈ 1.25e9 bytes > 2^30 (1 GiB); peak ≈50 KiB.
	q := "RETURN size(reduce(s = '', i IN range(1," + strconv.Itoa(k) + ") | s + 'a')) AS n"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		var ee *expr.EvalError
		if errors.As(err, &ee) {
			t.Logf("FIXED #1482: string-concat reduce rejected with typed EvalError at Run: %v", ee)
			return
		}
		t.Fatalf("Run(%q): unexpected non-EvalError: %v", q, err)
	}
	defer res.Close()
	for res.Next() { //nolint:revive // drain
	}
	iterErr := res.Err()
	var ee *expr.EvalError
	if errors.As(iterErr, &ee) {
		t.Logf("FIXED #1482: string-concat reduce rejected with typed EvalError: %v", ee)
		return
	}
	if errors.Is(iterErr, context.DeadlineExceeded) {
		t.Fatalf("Run(%q) hit the deadline instead of the byte budget: %v", q, iterErr)
	}
	t.Fatalf("Run(%q): completed without a byte-budget EvalError (iterErr=%v) — #1482 regressed", q, iterErr)
}

// TestSec_Cypher_StringConcat_PlainConcatBypassesBudget proves the byte budget
// is not specific to a bare reduce: the same growth driven from inside a list
// comprehension is equally charged, because the charge lives in evalArith's
// string branch (which every string-"+" path reaches). It asserts the typed
// *expr.EvalError just like the reduce form (#1482).
func TestSec_Cypher_StringConcat_PlainConcatBypassesBudget(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const k = 50000
	q := "RETURN [x IN [0] | size(reduce(s = '', i IN range(1," +
		strconv.Itoa(k) + ") | s + 'a'))] AS n"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		var ee *expr.EvalError
		if errors.As(err, &ee) {
			t.Logf("FIXED #1482 (comprehension path): %v", ee)
			return
		}
		t.Fatalf("Run(%q): unexpected non-EvalError: %v", q, err)
	}
	defer res.Close()
	for res.Next() { //nolint:revive // drain
	}
	iterErr := res.Err()
	var ee *expr.EvalError
	if errors.As(iterErr, &ee) {
		t.Logf("FIXED #1482 (comprehension path): %v", ee)
		return
	}
	if errors.Is(iterErr, context.DeadlineExceeded) {
		t.Fatalf("Run(%q) hit the deadline instead of the byte budget: %v", q, iterErr)
	}
	t.Fatalf("Run(%q): completed without a byte-budget EvalError (iterErr=%v) — #1482 regressed", q, iterErr)
}

// TestSec_Cypher_ListAccumulator_StillRejected is the POSITIVE lock-in: the
// LIST-accumulator doubling reduce that #1475 DID fix must keep failing fast.
// This guards a future string-fix from accidentally regressing the list path.
func TestSec_Cypher_ListAccumulator_StillRejected(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	// acc is a ListValue, so each concat IS charged — must trip the 1e7 budget.
	q := "RETURN size(reduce(acc=[0], i IN range(1,30) | acc + acc)) AS s"
	secCypherAssertListBudgetError(t, eng, q)
}
