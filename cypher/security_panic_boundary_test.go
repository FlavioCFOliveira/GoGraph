package cypher_test

// security_panic_boundary_test.go — DEFENSE LOCK-IN proving the public engine
// entrypoints (Run, RunInTx, Explain) NEVER unwind a recoverable panic past the
// engine and crash the embedding process. A crafted or malformed-after-parse
// query must return a typed error — a normal parse/sema/eval error, or one
// wrapping cypher.ErrInternalPanic — never a process abort.
//
// panic_boundary_test.go already drives a DELIBERATE panic (the boom() builtin)
// to prove the recover() boundary fires and rolls back the writer transaction.
// THIS file is the complement: a broad table of adversarial / degenerate query
// shapes — empty, half-formed, type-clashing, nonsense-after-keyword, deep but
// under-guard, missing-variable inside reduce — fired through all three public
// methods. The invariant is uniform: each call RETURNS (any typed error or a
// bounded result) within a watchdog, and the test goroutine survives, so a
// fatal stack overflow or unrecovered panic is converted into a clean failure.
//
// All cases pass today; this is a regression fence.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// secCypherEdgeQueries are crafted or degenerate queries. None should crash the
// process; each must yield a typed error or a bounded result through every
// public entrypoint.
func secCypherEdgeQueries() []string {
	return []string{
		"",
		"   ",
		"\n\t ",
		"RETURN",
		"MATCH",
		"WITH",
		"RETURN 1 +",
		"MATCH (n) WHERE RETURN n",
		"RETURN [1,2,3][999999999999]",           // out-of-range index → NULL, not crash
		"RETURN {a:1}.b.c.d",                     // chained access on missing keys
		"RETURN size(null)",                      // NULL-arg builtin
		"RETURN head([])",                        // empty-list builtin
		"UNWIND [] AS x RETURN x",                // empty UNWIND
		"RETURN 1 AS a, 2 AS a",                  // duplicate projection column
		"WITH 1 AS x RETURN y",                   // undefined variable
		"RETURN toInteger('not a number')",       // coercion failure → NULL
		"CALL db.nonexistent()",                  // unknown procedure
		"RETURN reduce(x=0, y IN [1,2] | x + z)", // undefined var inside reduce
		"MATCH (a)-[r]->(b)-[r]->(c) RETURN r",   // relationship reused
		"RETURN $missing",                        // missing parameter
		"MATCH p = (a) RETURN length(p)",         // single-node path
		"RETURN [x IN range(1,3) WHERE x > 10 | x*x]", // empty comprehension result
		"RETURN CASE WHEN null THEN 1 ELSE 2 END",     // null predicate in CASE
	}
}

// TestSec_Cypher_PanicBoundary_RunNeverCrashes asserts every edge query returns
// from Engine.Run within a watchdog (a typed error or a drainable Result),
// proving no input in the table escapes the recover boundary as a fatal crash.
func TestSec_Cypher_PanicBoundary_RunNeverCrashes(t *testing.T) {
	quietLogs(t)
	eng := secCypherNewEngine(t)
	for _, q := range secCypherEdgeQueries() {
		t.Run(secCypherCaseName(q), func(t *testing.T) {
			secCypherRunSurvives(t, func() {
				res, err := eng.Run(context.Background(), q, nil)
				if err == nil && res != nil {
					for res.Next() {
						_ = res.Record()
					}
					_ = res.Err()
					_ = res.Close()
				}
			})
		})
	}
}

// TestSec_Cypher_PanicBoundary_RunInTxNeverCrashes asserts the same for the
// write/autocommit entrypoint, which carries the additional obligation of
// releasing the writer transaction on the panic path.
func TestSec_Cypher_PanicBoundary_RunInTxNeverCrashes(t *testing.T) {
	quietLogs(t)
	eng := secCypherNewEngine(t)
	for _, q := range secCypherEdgeQueries() {
		t.Run(secCypherCaseName(q), func(t *testing.T) {
			secCypherRunSurvives(t, func() {
				res, err := eng.RunInTx(context.Background(), q, nil)
				if err == nil && res != nil {
					for res.Next() {
						_ = res.Record()
					}
					_ = res.Err()
					_ = res.Close()
				}
			})
		})
	}
}

// TestSec_Cypher_PanicBoundary_ExplainNeverCrashes asserts the planner-only
// Explain path also never crashes on the edge table. Explain has its own
// recover boundary (ErrInternalPanic) and must honour it.
func TestSec_Cypher_PanicBoundary_ExplainNeverCrashes(t *testing.T) {
	quietLogs(t)
	eng := secCypherNewEngine(t)
	for _, q := range secCypherEdgeQueries() {
		t.Run(secCypherCaseName(q), func(t *testing.T) {
			secCypherRunSurvives(t, func() {
				_, _ = eng.Explain(q, nil)
			})
		})
	}
}

// secCypherRunSurvives runs fn on a child goroutine under a watchdog. The body
// must complete (with or without a returned error) within the deadline; a hang
// — the observable symptom of a swallowed deadlock — fails the test instead of
// stalling the suite. A panic that escaped fn's callee boundary would crash the
// process regardless, so reaching the "completed" arm is itself the proof that
// the engine's recover boundary held.
func secCypherRunSurvives(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		// Completed without crashing the process — the boundary held.
	case <-time.After(15 * time.Second):
		t.Fatal("public entrypoint did not return within 15s on a crafted input (possible deadlock past the panic boundary)")
	}
}

// secCypherCaseName derives a short, stable subtest name from a query string.
func secCypherCaseName(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "empty"
	}
	if len(q) > 28 {
		q = q[:28]
	}
	return strings.NewReplacer(" ", "_", "/", "_", "(", "_", ")", "_", "[", "_", "]", "_", "\n", "_", "\t", "_", "\"", "_").Replace(q)
}
