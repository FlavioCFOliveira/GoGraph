package cypher_test

// result_rows_cap_test.go — black-box behavioural tests for the result-row cap
// (#1292). These exercise the public Engine API exactly as a consumer would:
//   - a test-lowered cap trips ErrResultRowsExceeded on a whole-graph MATCH,
//     surfaced via Result.Err() (the AC's bounded-resource error);
//   - the unlimited opt-out (MaxResultRowsUnlimited) returns every row with no
//     error, proving the explicit escape hatch still works.
//
// The default cap (DefaultMaxResultRows = 10M) is exercised cheaply here by
// lowering it through EngineOptions rather than building a 10M-node graph; the
// constructor wiring that proves the *default* itself is finite lives in the
// white-box result_rows_cap_internal_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestEngine_ResultRowCap_TripsBoundedError builds an engine whose row cap is
// lowered below the graph's node count and asserts that a whole-graph MATCH
// stops at the cap with ErrResultRowsExceeded reported by Result.Err(). This is
// the exact failure mode the AC describes (an unbounded MATCH on a large graph),
// reproduced at small scale via the public knob.
func TestEngine_ResultRowCap_TripsBoundedError(t *testing.T) {
	const (
		nodes = 20
		cap   = 5
	)
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cap})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if got := res.Err(); !errors.Is(got, cypher.ErrResultRowsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrResultRowsExceeded", got)
	}
	// Next stops returning rows once the cap is exceeded; the consumer must not
	// have drained more than the cap's worth of rows.
	if count > cap {
		t.Fatalf("drained %d rows, want <= cap %d before the error tripped", count, cap)
	}
}

// TestEngine_ResultRowCap_DefaultAllowsSmallResult confirms the finite default
// (DefaultMaxResultRows) does not interfere with ordinary, small results: a
// MATCH over a handful of nodes returns every row with no error. This is the
// guard that the default is high enough for the TCK and the examples.
func TestEngine_ResultRowCap_DefaultAllowsSmallResult(t *testing.T) {
	const nodes = 7
	g := newGraph(t, nodes)
	eng := cypher.NewEngine(g) // default cap

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil under the default cap", err)
	}
	if count != nodes {
		t.Fatalf("got %d rows, want %d", count, nodes)
	}
}

// TestEngine_ResultRowCap_UnlimitedOptOut proves the explicit opt-out: setting
// MaxResultRows to the unlimited sentinel disables the cap, so a whole-graph
// MATCH over a node count that would trip a small cap returns every row with no
// error. Without the opt-out the same node count under a low cap errors (see
// TestEngine_ResultRowCap_TripsBoundedError); here the sentinel removes it.
func TestEngine_ResultRowCap_UnlimitedOptOut(t *testing.T) {
	const nodes = 50
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultRows: cypher.MaxResultRowsUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil for the unlimited opt-out", err)
	}
	if count != nodes {
		t.Fatalf("got %d rows, want %d (opt-out must return every row)", count, nodes)
	}
}
