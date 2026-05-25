package cypher_test

// remove_property_test.go — additive REMOVE property tests (T834).
//
// Existing TestRunInTx_RemoveProperty covers the basic case (remove an
// existing property, verify absence). Tests here cover edge cases:
//   - removing a property that does not exist (no error, no-op)
//   - removing one of multiple properties, others remain
//   - RETURN of a surviving property after a sibling is removed

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestRemove_NonExistentProperty verifies that REMOVE n.age on a node that
// has no "age" property succeeds silently (no error, existing props intact).
func TestRemove_NonExistentProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person {name: "Alice"})`)

	// Remove a property that was never set — must not error.
	drainRunInTx(t, eng, `MATCH (n:Person) REMOVE n.age`)

	// "name" property must still be intact.
	res, err := eng.RunInTx(context.Background(), `MATCH (n:Person) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("RETURN n.name after REMOVE n.age: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok || string(got) != "Alice" {
		t.Errorf("n.name = %v (%T), want StringValue(Alice)", rows[0]["n.name"], rows[0]["n.name"])
	}
}

// TestRemove_OneOfMultipleProperties removes one property from a node that
// carries two properties and verifies the other property is not disturbed.
func TestRemove_OneOfMultipleProperties(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person {name: "Bob", score: 42})`)
	drainRunInTx(t, eng, `MATCH (n:Person) REMOVE n.score`)

	// "score" must be gone.
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		props := g.NodeProperties(key)
		if _, ok := props["score"]; ok {
			t.Errorf("score property still present after REMOVE n.score")
			return false
		}
		return true
	})

	// "name" must still be "Bob".
	res, err := eng.RunInTx(context.Background(), `MATCH (n:Person) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("RETURN n.name: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok || string(got) != "Bob" {
		t.Errorf("n.name = %v (%T), want StringValue(Bob)", rows[0]["n.name"], rows[0]["n.name"])
	}
}

// TestRemove_ReturnSurvivingProperty runs MATCH … REMOVE n.age RETURN n.score
// in a single query and verifies that the returned score value is intact.
func TestRemove_ReturnSurvivingProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Player {age: 30, score: 99})`)

	res, err := eng.RunInTx(context.Background(), `MATCH (n:Player) REMOVE n.age RETURN n.score`, nil)
	if err != nil {
		t.Fatalf("REMOVE n.age RETURN n.score: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["n.score"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("n.score: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 99 {
		t.Errorf("n.score = %d, want 99", int64(got))
	}
}
