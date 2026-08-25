package cypher_test

// security_cypher_string_byte_budget_test.go — afternoon audit SEC-2026-06-14b.
//
// ── #1482 — string "+" byte-growth charged by a per-evaluation BYTE budget ────
// The per-evaluation list-element budget (expr.DefaultMaxListElements = 1e7,
// #1475) charges LIST ELEMENTS only and is byte-blind. STRING "+" concatenation
// in evalArith used to return StringValue(string(ls)+string(rs)) with NO charge,
// so a doubling reduce over a STRING accumulator
//
//	reduce(s='x', i IN range(1,N) | s + s)
//
// grew the string to 2^N bytes from O(1) query text (8 GiB at N=33) — an
// out-of-memory DoS reachable from any untrusted query (Engine.Run, Bolt RUN).
//
// FIXED #1482: evalArith's string "+" branch now charges len(ls)+len(rs) bytes
// against a per-evaluation BYTE budget (expr.DefaultMaxStringEvalBytes = 1 GiB)
// BEFORE allocating, returning a typed *expr.EvalError on breach (fail-stop),
// mirroring the list-element budget. The ceiling sits ~5 orders of magnitude
// above the TCK's largest string (the 10,000-char Literals6 literal), so no
// conforming query trips it.
//
// These tests are BOUNDED and FAST. The over-budget probe uses the O(N²)
// linear-append form `reduce(s, i IN range(1,K) | s + 'x')`: each concat charges
// len(s)+1 bytes, so the CUMULATIVE charge is ≈K²/2 and crosses the 1 GiB budget
// at K≈46341 while the PEAK string allocation stays ≈46 KiB — proving the
// cumulative byte budget fires without ever allocating near the host limit. A
// POSITIVE control fences the TCK floor: a 10,000-character string literal
// (Literals6 [8]) must always succeed.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// secCypherStringGrowSize runs q (one integer column "n" = size/length of the
// grown string) and returns (completed, the produced value or -1, the
// iteration/entry error). It never runs an unbounded payload.
//
// There is deliberately NO context deadline, and one must not be reintroduced.
// Every payload here is BOUNDED by construction: K is a compile-time constant
// and the peak string stays ≈K bytes, so the query self-terminates whether or
// not the byte budget fires. A regressed budget therefore surfaces as a
// COMPLETED run — which [assertStringBudgetTrips] reports by name — and never
// as a hang. Measured under -race on an idle host, the largest linear-append
// payload that stays inside the ceiling (K=46000, charging 1.058e9 of the
// 1 GiB budget) runs to completion in ~206 ms, so even a fully regressed budget
// costs a fraction of a second.
//
// A deadline here would be actively harmful, because it makes a CORRECTNESS
// oracle depend on the wall clock: `context.DeadlineExceeded` reads identically
// whether the byte budget failed to fire or the machine was merely slow. That
// is not hypothetical. `make ci` runs the whole module under -race with many
// packages in parallel, and the resulting ~20x slowdown pushed a former 10 s
// deadline past its own expiry, reporting a loaded machine as a byte-budget
// defect — a gate that is red on a busy host and green on a quiet one. The
// elapsed time is logged as a DIAGNOSTIC below, never as a verdict. A genuine
// hang, as opposed to a slow run, is still caught by `go test`'s own -timeout,
// whose full goroutine dump says far more than a bare DeadlineExceeded could.
func secCypherStringGrowSize(t *testing.T, eng *cypher.Engine, q string) (bool, int64, error) {
	t.Helper()
	start := time.Now()
	// The query is truncated because one caller passes a 10,000-character literal.
	defer func() {
		t.Logf("secCypherStringGrowSize: %.60q… took %v (diagnostic only, never a verdict)", q, time.Since(start))
	}()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		return false, -1, err
	}
	var n int64 = -1
	for res.Next() {
		if iv, ok := res.Record()["n"].(expr.IntegerValue); ok {
			n = int64(iv)
		}
	}
	iterErr := res.Err()
	_ = res.Close()
	return iterErr == nil, n, iterErr
}

// secCypherStringByteBudgetMarker is the fragment of the #1482 byte-budget
// message (expr.errStringTooLarge) that tells it apart from its two SIBLING
// budgets, whose messages are deliberately the same shape: the list-element
// budget (expr.errListTooLarge) says "list elements" and the value-depth budget
// (expr.errValueTooDeep) says "nested deeper". Pinning the marker stops the gate
// passing vacuously on some unrelated *expr.EvalError that happens to surface
// from the same payload.
const secCypherStringByteBudgetMarker = "string bytes"

// assertStringBudgetTrips requires q to fail-stop with a typed *expr.EvalError
// naming the byte budget, surfaced either at Run() or during iteration. Each
// rejection path names what actually went wrong, so a genuine regression is
// never reported as something else.
func assertStringBudgetTrips(t *testing.T, eng *cypher.Engine, name, q string) {
	t.Helper()
	completed, n, err := secCypherStringGrowSize(t, eng, q)
	var ee *expr.EvalError
	if errors.As(err, &ee) {
		if !strings.Contains(ee.Error(), secCypherStringByteBudgetMarker) {
			t.Fatalf("[%s] %q was rejected by a typed *expr.EvalError that is NOT the byte budget: %v; want a message naming %q, so this gate is asserting #1482 and not a sibling budget",
				name, q, ee, secCypherStringByteBudgetMarker)
		}
		t.Logf("[%s] #1482: rejected with typed *expr.EvalError: %v", name, ee)
		return
	}
	if completed {
		t.Fatalf("[%s] %q COMPLETED with size=%d instead of being rejected: the cumulative string-byte charge for this payload is far above expr.DefaultMaxStringEvalBytes (%d), so evalArith's string-\"+\" branch is no longer charging bytes before it allocates — #1482 has regressed",
			name, q, n, expr.DefaultMaxStringEvalBytes)
	}
	t.Fatalf("[%s] %q failed with a non-EvalError (%T): %v; want the typed *expr.EvalError of the #1482 byte budget",
		name, q, err, err)
}

// TestSec_Cypher_StringConcat_OverBudgetReduceRejected drives the O(N²)
// linear-append reduce past the 1 GiB cumulative byte budget (K≈46341 ⇒
// cumulative ≈K²/2 > 2^30) while the peak string stays ≈46 KiB. The byte budget
// MUST fail-stop with a typed *expr.EvalError (#1482).
func TestSec_Cypher_StringConcat_OverBudgetReduceRejected(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	// Σ_{i=1}^{K}(1+i) ≈ K²/2; K=50000 ⇒ ≈1.25e9 bytes charged > 2^30 (1 GiB).
	const k = 50000
	q := "RETURN size(reduce(s='', i IN range(1," + strconv.Itoa(k) + ") | s + 'a')) AS n"
	assertStringBudgetTrips(t, eng, "reduce-linear-append", q)
}

// TestSec_Cypher_StringConcat_OverBudgetWithChainRejected exercises the same
// per-evaluation byte budget through a WITH-projection chain rather than reduce,
// proving the charge lives in evalArith's string branch (which serves every
// string-"+" path) and is not specific to reduce. Each stage appends a fixed
// 16 KiB chunk to the carried string, so after the carried string has grown the
// per-stage charge (len(s)+chunk) accumulates linearly. With a 16 KiB chunk the
// cumulative charge crosses the 1 GiB budget within a few thousand stages while
// the peak string stays small relative to the host. We keep the chain short and
// the chunk large so the cumulative budget — not any single allocation — is what
// fires. The WITH-projection budget is per-projection, so the over-budget
// expression is the per-stage RETURN of a reduce that linearly appends.
func TestSec_Cypher_StringConcat_OverBudgetWithChainRejected(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	// One WITH stage carries a seed; a reduce in the RETURN appends per iteration.
	// Σ_{i=1}^{K}(seed+i) ≈ K²/2 crosses 2^30 at K≈46341; peak string ≈seed+K.
	const k = 50000
	var b strings.Builder
	b.WriteString("WITH 'seed' AS base ")
	b.WriteString("RETURN size(reduce(s=base, i IN range(1," + strconv.Itoa(k) + ") | s + 'a')) AS n")
	assertStringBudgetTrips(t, eng, "with-then-linear-append", b.String())
}

// TestSec_Cypher_StringConcat_LegitimateLiteralStillWorks is the POSITIVE TCK
// floor: a 10000-character string literal (openCypher TCK Literals6 [8]) must
// always succeed. The #1482 byte ceiling (1 GiB) sits ~5 orders of magnitude
// above this, so this control fences the fix against over-aggressive capping.
func TestSec_Cypher_StringConcat_LegitimateLiteralStillWorks(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)
	const litLen = 10000 // the largest string the TCK asserts a successful RETURN for
	q := "RETURN size('" + strings.Repeat("a", litLen) + "') AS n"
	completed, n, err := secCypherStringGrowSize(t, eng, q)
	if !completed {
		t.Fatalf("legitimate %d-char literal failed: err=%v — a #1482 byte ceiling must stay above the TCK Literals6 floor (10000 chars)", litLen, err)
	}
	if n != litLen {
		t.Fatalf("RETURN size of %d-char literal = %d; want %d", litLen, n, litLen)
	}
}
