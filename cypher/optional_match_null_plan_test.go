package cypher_test

// optional_match_null_plan_test.go — T922: OPTIONAL MATCH null propagation in
// the logical plan and at runtime.
//
// Acceptance criteria:
//  1. engine.Explain returns a plan containing "OptionalApply" or
//     "OptionalExpand".
//  2. engine.Run preserves null-bearing outer rows (nodes with no matching
//     outgoing edge emit a row with expr.IsNull(b.name) == true).
//  3. Race-clean.
//  4. goleak-clean (enforced by TestMain in testmain_test.go).
//
// Plan operator selection (from ir/optional_match_test.go):
//   - MATCH … OPTIONAL MATCH … (chained) → OptionalApply when the driving
//     plan is non-empty.
//   - Standalone OPTIONAL MATCH (n)-[r]->(m) → OptionalExpand.
//
// The tests use CREATE to set up graph data so that relationship types are
// fully registered in the engine, matching the pattern used throughout the
// cypher test suite.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newOptionalMatchEngine returns an engine loaded with:
//
//	(:Person {name:'alice'})-[:KNOWS]->(:Person {name:'bob'})
//	(:Person {name:'carol'})   (isolated, no outgoing KNOWS)
func newOptionalMatchEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Person {name: 'alice'})-[:KNOWS]->(:Person {name: 'bob'})`)
	runSetup(t, eng, `CREATE (:Person {name: 'carol'})`)
	return eng
}

// TestOptionalMatchNullPlan_ExplainContainsOptionalOperator verifies that the
// planner emits an Optional* operator for an OPTIONAL MATCH query chained
// after a MATCH.
func TestOptionalMatchNullPlan_ExplainContainsOptionalOperator(t *testing.T) {
	t.Parallel()

	eng := newOptionalMatchEngine(t)

	const q = `MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b:Person) RETURN a.name, b.name`
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if plan == "" {
		t.Fatal("Explain returned empty plan")
	}

	hasOptional := strings.Contains(plan, "OptionalApply") ||
		strings.Contains(plan, "OptionalExpand")
	if !hasOptional {
		t.Errorf("plan contains neither OptionalApply nor OptionalExpand:\n%s", plan)
	}
}

// TestOptionalMatchNullPlan_NullPropagation verifies execution null semantics:
// rows whose outer node has no matching KNOWS edge must carry a null b.name.
func TestOptionalMatchNullPlan_NullPropagation(t *testing.T) {
	t.Parallel()

	eng := newOptionalMatchEngine(t)
	ctx := context.Background()

	const q = `MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b:Person) RETURN a.name, b.name`
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// 3 Person nodes: alice has KNOWS→bob, bob and carol have none.
	// Expected: 3 rows total — (alice, bob), (bob, null), (carol, null).
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", len(rows), rows)
	}

	nullCount := 0
	nonNullCount := 0
	for _, row := range rows {
		// a.name must always be a non-empty string.
		av, ok := row["a.name"].(expr.StringValue)
		if !ok {
			t.Errorf("a.name expected StringValue, got %T (%v)", row["a.name"], row["a.name"])
		} else if string(av) == "" {
			t.Errorf("a.name is empty")
		}

		// b.name is null for nodes without an outgoing KNOWS edge.
		bv, ok := row["b.name"].(expr.Value)
		if !ok {
			t.Fatalf("b.name is not expr.Value: %T", row["b.name"])
		}
		if expr.IsNull(bv) {
			nullCount++
		} else {
			nonNullCount++
			sv, ok := bv.(expr.StringValue)
			if !ok {
				t.Fatalf("b.name non-null but not StringValue: %T (%v)", bv, bv)
			}
			if string(sv) != "bob" {
				t.Errorf("non-null b.name = %q, want %q", string(sv), "bob")
			}
		}
	}

	if nullCount != 2 {
		t.Errorf("null b.name count = %d, want 2", nullCount)
	}
	if nonNullCount != 1 {
		t.Errorf("non-null b.name count = %d, want 1", nonNullCount)
	}
}

// TestOptionalMatchNullPlan_AllIsolated verifies null propagation when no node
// in the graph has any outgoing KNOWS edge: every row must have null b.name.
func TestOptionalMatchNullPlan_AllIsolated(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, name := range []string{"x", "y", "z"} {
		runSetup(t, eng, fmt.Sprintf(`CREATE (:Person {name: '%s'})`, name))
	}
	ctx := context.Background()

	const q = `MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b:Person) RETURN a.name, b.name`
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, row := range rows {
		bv, ok := row["b.name"].(expr.Value)
		if !ok {
			t.Fatalf("row %d: b.name not expr.Value: %T", i, row["b.name"])
		}
		if !expr.IsNull(bv) {
			t.Errorf("row %d: b.name expected NULL, got %T (%v)", i, bv, bv)
		}
	}
}

// TestOptionalMatchNullPlan_RaceClean runs concurrent Explain and Run calls to
// satisfy the race-clean acceptance criterion.
func TestOptionalMatchNullPlan_RaceClean(t *testing.T) {
	t.Parallel()

	eng := newOptionalMatchEngine(t)
	ctx := context.Background()

	const q = `MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b:Person) RETURN a.name, b.name`
	const workers = 8
	done := make(chan struct{}, workers)
	errs := make(chan error, workers*2)

	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := eng.Explain(q, nil); err != nil {
				errs <- fmt.Errorf("Explain: %w", err)
				return
			}
			res, err := eng.Run(ctx, q, nil)
			if err != nil {
				errs <- fmt.Errorf("Run: %w", err)
				return
			}
			rows := collectRecords(t, res)
			if len(rows) != 3 {
				errs <- fmt.Errorf("expected 3 rows, got %d", len(rows))
			}
		}()
	}

	for range workers {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
