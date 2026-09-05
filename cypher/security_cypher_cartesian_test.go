package cypher_test

// security_cypher_cartesian_test.go — afternoon audit SEC-2026-06-14b.
//
// ── #1483 — disconnected Cartesian product: surface the risk + lock in ─────────
// ── deadline cancellation ──────────────────────────────────────────────────────
// A disconnected Cartesian-product MATCH (a),(b),(c),... with no shared variable
// streams N^k intermediate tuples through the pipeline. When the final operator
// collapses cardinality (count(*), or a write), the result-rows / result-bytes
// caps NEVER fire because the OUTPUT is one row, yet the engine spends O(N^k) CPU
// producing tuples (CWE-400 / CWE-770).
//
// MITIGATION (the decision implemented for #1483, two parts):
//
//  1. SURFACE the risk: the planner now emits a plan-time Cartesian-product
//     NOTIFICATION (Neo.ClientNotification.Statement.CartesianProductWarning,
//     mirroring Neo4j) for a disconnected pattern. Notifications are out of band
//     (not result rows), so this is openCypher-TCK-safe. It is exposed via
//     cypher.Result.Notifications() and, on the Bolt server, in the terminal PULL
//     SUCCESS "notifications" metadata.
//  2. LOCK IN cancellation: the operator path is cancellable (Apply/scan poll
//     ctx.Err() at every Next and every 4096 tuples), so a caller deadline aborts
//     the product promptly. The test below fences that contract.
//
// All inputs are BOUNDED: small node counts and a short deadline; the test never
// runs a payload that could exhaust the host.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const cartesianNotificationCode = "Neo.ClientNotification.Statement.CartesianProductWarning"

// secCartesianEngine builds an engine seeded with n labelled nodes via the Go API
// (no CREATE round-trip), so the Cartesian product has a real binding set.
func secCartesianEngine(t *testing.T, n int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %q: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "N"); err != nil {
			t.Fatalf("SetNodeLabel %q: %v", key, err)
		}
	}
	return cypher.NewEngine(g)
}

// runNotifications runs q and returns the plan-time notifications attached to the
// result. The query is fully drained and the result closed.
//
// No deadline: the callers run over a 4-node graph, so the largest product here
// is 4^4 = 256 tuples — bounded and sub-millisecond. A wall-clock ceiling would
// add no detection power and would make a NOTIFICATION-shape assertion depend on
// how busy the machine is, reporting a loaded host as a plan-notification defect.
// See the rationale on secCypherStringGrowSize in
// security_cypher_string_byte_budget_test.go.
func runNotifications(t *testing.T, eng *cypher.Engine, q string) []cypher.Notification {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	for res.Next() { // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate %q: %v", q, err)
	}
	ns := res.Notifications()
	_ = res.Close()
	return ns
}

// hasCartesianNotification reports whether ns contains the Cartesian-product
// notification.
func hasCartesianNotification(ns []cypher.Notification) bool {
	for _, n := range ns {
		if n.Code == cartesianNotificationCode {
			return true
		}
	}
	return false
}

// TestSec_Cypher_Cartesian_EmitsNotification asserts the #1483 fix: a
// disconnected pattern produces the Cartesian-product notification (the risk is
// surfaced), and a connected pattern (shared variable or WHERE join predicate)
// does NOT — so the warning is faithful and not spuriously raised.
func TestSec_Cypher_Cartesian_EmitsNotification(t *testing.T) {
	t.Parallel()
	eng := secCartesianEngine(t, 4)

	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"disconnected_4way", "MATCH (a:N),(b:N),(c:N),(d:N) RETURN count(*) AS c", true},
		{"disconnected_2way", "MATCH (a:N),(b:N) RETURN count(*) AS c", true},
		{"disconnected_sequential_match", "MATCH (a:N) MATCH (b:N) RETURN count(*) AS c", true},
		{"connected_by_relationship", "MATCH (a:N)-[r]->(b:N) RETURN count(*) AS c", false},
		{"connected_by_where_join", "MATCH (a:N),(b:N) WHERE a = b RETURN count(*) AS c", false},
		{"single_pattern", "MATCH (a:N) RETURN count(*) AS c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := runNotifications(t, eng, tc.q)
			got := hasCartesianNotification(ns)
			if got != tc.want {
				t.Fatalf("%q: cartesian notification present=%v, want %v (notifications=%+v)", tc.q, got, tc.want, ns)
			}
			if got {
				// The notification must carry the Neo4j-faithful shape.
				var note cypher.Notification
				for _, n := range ns {
					if n.Code == cartesianNotificationCode {
						note = n
					}
				}
				if note.Title == "" || note.Severity != "INFORMATION" || note.Category != "PERFORMANCE" {
					t.Fatalf("%q: notification missing Neo4j-faithful fields: %+v", tc.q, note)
				}
				if !strings.Contains(note.Description, "cartesian product") {
					t.Fatalf("%q: notification description does not describe the cross product: %q", tc.q, note.Description)
				}
			}
		})
	}
}

// TestSec_Cypher_Cartesian_IsCancellable is the locked-in cancellation contract
// that bounds the #1483 CPU-exhaustion vector: a 5-way disconnected Cartesian
// product over 60 nodes (60^5 ≈ 7.78e8 tuples — minutes-to-hours uncancellable)
// MUST abort promptly under a sub-second caller deadline with
// context.DeadlineExceeded. If this ever stops aborting, the deadline mitigation
// for the absent default budget is gone.
func TestSec_Cypher_Cartesian_IsCancellable(t *testing.T) {
	t.Parallel()
	eng := secCartesianEngine(t, 60)

	const deadline = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	q := "MATCH (a:N),(b:N),(c:N),(d:N),(e:N) RETURN count(*) AS c"
	t0 := time.Now()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		// A pre-iteration deadline rejection is a legitimate prompt abort.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return
		}
		t.Fatalf("Run(%q): unexpected entry error %v", q, err)
	}
	for res.Next() { // drain until the deadline aborts the product
	}
	elapsed := time.Since(t0)
	iterErr := res.Err()
	_ = res.Close()

	if !errors.Is(iterErr, context.DeadlineExceeded) {
		t.Fatalf("#1483 deadline mitigation GONE: 5-way Cartesian product under a %v deadline returned err=%v after %v; want context.DeadlineExceeded — the operator path no longer honours cancellation, so an untrusted disconnected MATCH is an unbounded CPU-exhaustion DoS", deadline, iterErr, elapsed)
	}
	// The abort must be prompt — not the full N^5 traversal.
	if elapsed > 5*time.Second {
		t.Fatalf("#1483 deadline mitigation WEAK: Cartesian product took %v under a %v deadline; the ctx poll stride may be too coarse for the Apply/scan chain", elapsed, deadline)
	}
	t.Logf("Cartesian product (60^5 ≈ 7.78e8 tuples) aborted in %v under a %v deadline (context.DeadlineExceeded).", elapsed, deadline)
}

// TestSec_Cypher_Cartesian_FinalRowCapDoesNotBoundIntermediateWork documents the
// core of #1483: the result-rows cap bounds only OUTPUT rows, so a Cartesian
// product collapsed by count(*) to ONE row sails past it while having streamed
// N^k tuples. Here a 4-way product over 40 nodes streams 40^4 = 2.56M tuples yet
// returns a single row well under DefaultMaxResultRows (1e7). This is why the
// notification (surfacing the risk) and the deadline (bounding it) are the
// chosen mitigation rather than the output-row cap. Bounded (≈100ms).
func TestSec_Cypher_Cartesian_FinalRowCapDoesNotBoundIntermediateWork(t *testing.T) {
	t.Parallel()
	const n = 40
	eng := secCartesianEngine(t, n)

	// No deadline. This test asserts the 2.56M-tuple product *completes* (all
	// intermediate tuples streamed, one row out, notification emitted), so the
	// payload is BOUNDED by construction and self-terminating: 40^4 tuples of
	// counting, and nothing here can wait on anything. A completion ceiling was
	// therefore never a security bound, only a harness ceiling — and a harness
	// ceiling on a bounded payload buys no detection at all, because the
	// assertions below already fail when the count or the notification is wrong.
	//
	// It did, however, cost correctness. Expiry surfaced as `t.Fatalf("iterate:
	// context deadline exceeded")`, which reads identically whether the engine
	// broke or the machine was busy. This test has already flaked that way once
	// and was "fixed" by widening the ceiling from a tighter value to 120s —
	// treating the symptom. Widening only moves the threshold; the sibling
	// string-byte-budget gate blew a 10s ceiling under the parallel -race load of
	// `make ci` all the same. Removing the wall clock from the oracle is what
	// actually makes the gate deterministic. A genuine non-terminating regression
	// is still caught by `go test`'s own -timeout, with a full goroutine dump.
	ctx := context.Background()

	q := "MATCH (a:N),(b:N),(c:N),(d:N) RETURN count(*) AS c"
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	var c int64 = -1
	rows := 0
	for res.Next() {
		rows++
		if iv, ok := res.Record()["c"].(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	// The 4-way disconnected product must also carry the notification.
	if !hasCartesianNotification(res.Notifications()) {
		t.Fatalf("4-way disconnected product did not emit the Cartesian-product notification")
	}
	_ = res.Close()

	want := int64(n) * int64(n) * int64(n) * int64(n) // n to the fourth power: 2_560_000 when n is 40
	if c != want {
		t.Fatalf("count(*) over 4-way Cartesian = %d; want %d (the product semantics changed)", c, want)
	}
	if rows != 1 {
		t.Fatalf("expected a single output row from count(*); got %d", rows)
	}
	t.Logf("#1483: 4-way Cartesian streamed %d tuples through count(*) yet returned 1 row (<< DefaultMaxResultRows=1e7); the row/byte caps do NOT bound intermediate N^k work — hence the notification + deadline mitigation.", want)
}
