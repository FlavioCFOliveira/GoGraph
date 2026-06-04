package cypher_test

// collect_items_cap_test.go — black-box behavioural tests for the per-group
// element budget on buffering aggregators (#1294). These exercise the public
// Engine API exactly as a consumer would:
//   - a test-lowered budget trips funcs.ErrCollectItemsExceeded on a
//     `collect()` over a whole-graph MATCH, surfaced via Result.Err() (the AC's
//     bounded-resource error);
//   - the unlimited opt-out (MaxCollectItemsUnlimited) collects every value with
//     no error, proving the explicit escape hatch works;
//   - the finite default does not interfere with an ordinary small collect.
//
// The default budget (DefaultMaxCollectItems = 10M) is exercised cheaply here by
// lowering it through EngineOptions rather than building a 10M-node graph; the
// operator-level proof that the cap trips during the blocking consume phase
// lives in the white-box exec/eager_aggregation_budget_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// TestEngine_CollectItemsCap_TripsBoundedError builds an engine whose per-group
// element budget is lowered below the graph's node count and asserts that
// `MATCH (n) RETURN collect(n)` — a single grouping-key-free group — stops with
// ErrCollectItemsExceeded reported by Result.Err(). This reproduces the AC's
// failure mode (an unbounded collect on a large graph) at small scale via the
// public knob.
func TestEngine_CollectItemsCap_TripsBoundedError(t *testing.T) {
	const (
		nodes  = 20
		budget = 5
	)
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxCollectItems: budget})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN collect(n)", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() {
	}
	if got := res.Err(); !errors.Is(got, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrCollectItemsExceeded", got)
	}
}

// TestEngine_CollectItemsCap_PercentileTripsBoundedError mirrors the collect
// case for a percentile aggregator, which buffers all values just like collect.
func TestEngine_CollectItemsCap_PercentileTripsBoundedError(t *testing.T) {
	const (
		nodes  = 20
		budget = 5
	)
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxCollectItems: budget})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN percentileCont(id(n), 0.5)", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() {
	}
	if got := res.Err(); !errors.Is(got, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrCollectItemsExceeded", got)
	}
}

// TestEngine_CollectItemsCap_DistinctTripsBoundedError confirms the budget also
// bounds collect(DISTINCT …): the DISTINCT wrapper forwards each distinct value
// to the inner CollectAgg, whose budget still applies.
func TestEngine_CollectItemsCap_DistinctTripsBoundedError(t *testing.T) {
	const (
		nodes  = 20
		budget = 5
	)
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxCollectItems: budget})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN collect(DISTINCT id(n))", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	for res.Next() {
	}
	if got := res.Err(); !errors.Is(got, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("Result.Err() = %v, want ErrCollectItemsExceeded", got)
	}
}

// TestEngine_CollectItemsCap_DefaultAllowsSmallCollect confirms the finite
// default (DefaultMaxCollectItems) does not interfere with an ordinary, small
// collect: `collect(n)` over a handful of nodes returns the full list with no
// error. This is the guard that the default is high enough for the TCK and the
// examples.
func TestEngine_CollectItemsCap_DefaultAllowsSmallCollect(t *testing.T) {
	const nodes = 7
	g := newGraph(t, nodes)
	eng := cypher.NewEngine(g) // default budget

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN collect(n) AS xs", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rows++
		rec := res.Record()
		xs, ok := rec["xs"].(expr.ListValue)
		if !ok {
			t.Fatalf("xs is %T, want expr.ListValue", rec["xs"])
		}
		if len(xs) != nodes {
			t.Fatalf("collected list has %d elements, want %d", len(xs), nodes)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil under the default budget", err)
	}
	if rows != 1 {
		t.Fatalf("got %d rows, want exactly 1 global-aggregate row", rows)
	}
}

// TestEngine_CollectItemsCap_UnlimitedOptOut proves the explicit opt-out:
// setting MaxCollectItems to the unlimited sentinel disables the budget, so a
// `collect(n)` over a node count that would trip a small budget returns the full
// list with no error.
func TestEngine_CollectItemsCap_UnlimitedOptOut(t *testing.T) {
	const nodes = 50
	g := newGraph(t, nodes)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxCollectItems: cypher.MaxCollectItemsUnlimited,
	})

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN collect(n) AS xs", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rows++
		rec := res.Record()
		if xs, ok := rec["xs"].(expr.ListValue); !ok || len(xs) != nodes {
			t.Fatalf("opt-out: xs = %v, want %d-element list", rec["xs"], nodes)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err() = %v, want nil for the unlimited opt-out", err)
	}
	if rows != 1 {
		t.Fatalf("got %d rows, want exactly 1", rows)
	}
}
