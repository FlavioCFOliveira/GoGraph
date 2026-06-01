package cypher_test

// set_multiple_properties_test.go — T824
//
// Tests for setting multiple properties in a single SET clause. These are new
// tests not covered by any existing file.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSet_MultiplePropertiesAtOnce verifies that a single SET clause with
// multiple property assignments persists all of them correctly.
func TestSet_MultiplePropertiesAtOnce(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Profile {name: "Alice"})`)
	drainRunInTx(t, eng,
		`MATCH (n:Profile {name: "Alice"}) SET n.age = 30, n.score = 9.5, n.active = true`)

	// Walk and collect properties for the Alice node.
	type snap struct {
		age    int64
		score  float64
		active bool
		found  bool
	}
	var s snap
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		props := g.NodeProperties(key)
		nv, ok := props["name"]
		if !ok {
			return true
		}
		sv, ok := nv.String()
		if !ok || sv != "Alice" {
			return true
		}
		s.found = true
		if av, ok2 := props["age"]; ok2 {
			s.age, _ = av.Int64()
		}
		if scv, ok2 := props["score"]; ok2 {
			s.score, _ = scv.Float64()
		}
		if actv, ok2 := props["active"]; ok2 {
			s.active, _ = actv.Bool()
		}
		return false
	})

	if !s.found {
		t.Fatal("node with name=Alice not found after SET")
	}
	if s.age != 30 {
		t.Errorf("age = %d, want 30", s.age)
	}
	if s.score != 9.5 {
		t.Errorf("score = %f, want 9.5", s.score)
	}
	if !s.active {
		t.Errorf("active = %v, want true", s.active)
	}
}

// TestSet_MultiplePropertiesViaReturn verifies the same three-property SET
// scenario using a RETURN clause so the values come back through the result
// pipeline.
func TestSet_MultiplePropertiesViaReturn(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Profile {name: "Bob"})`)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:Profile {name: "Bob"})
		 SET n.age = 25, n.score = 7.0, n.active = false
		 RETURN n.age, n.score, n.active`, nil)
	if err != nil {
		t.Fatalf("RunInTx SET+RETURN: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]

	if got := fmtAny(row["n.age"]); got != "25" {
		t.Errorf("n.age = %s, want 25", got)
	}
	// score may be 7 or 7.0 depending on numeric representation.
	scoreStr := fmtAny(row["n.score"])
	if scoreStr != "7" && scoreStr != "7.0" {
		t.Errorf("n.score = %s, want 7 or 7.0", scoreStr)
	}
	if got := fmtAny(row["n.active"]); got != "false" {
		t.Errorf("n.active = %s, want false", got)
	}
}
