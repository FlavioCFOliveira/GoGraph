package cypher_test

// inline_props_sema_test.go — pins the precondition that makes the
// non-MapLiteral fallback in ir.buildPropertySelection safe (rmp #2272).
//
// Layer: short.
//
// A Selection built without a parsed predicate is evaluated by a PASS-THROUGH
// STUB, so it matches every row. That mechanism is what silently dropped the
// inline WHERE of a projected pattern comprehension. ir.buildPropertySelection
// contains a structurally identical fallback for an inline property map that is
// not a map literal, and its safety rests entirely on a claim about a DIFFERENT
// layer: that semantic analysis rejects every such form before a plan is built.
//
// An unverified claim of that kind is how the pattern-comprehension defect
// survived. This test turns it into a gate: if semantic analysis is ever
// relaxed to admit a parameter as an inline property map, this fails, and
// whoever makes that change is told that the planner fallback must be given a
// real predicate first — instead of shipping a query that silently matches
// every node.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestBuildPropertySelection_ParameterIsRejectedBySema(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"alice", "bob", "carol"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(n, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.AddEdgeLabeled("alice", "bob", 1, "K"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	eng := cypher.NewEngine(g)
	params := map[string]expr.Value{
		"p": expr.MapValue(map[string]expr.Value{"name": expr.StringValue("alice")}),
	}

	// A parameter in the inline-property position must be REFUSED, not planned.
	// Were it planned, the fallback would produce a pass-through Selection and
	// the query would silently return all three nodes.
	for _, q := range []string{
		`MATCH (n:P $p) RETURN count(*) AS n`,
		`MATCH ()-[r:K $p]->() RETURN count(*) AS n`,
	} {
		res, err := eng.Run(context.Background(), q, params)
		if err == nil {
			if res != nil {
				for res.Next() { //nolint:revive // full drain
				}
				err = res.Err()
				_ = res.Close()
			}
		}
		if err == nil {
			t.Fatalf("%s was ACCEPTED. ir.buildPropertySelection's non-MapLiteral fallback is now "+
				"reachable and builds a pass-through Selection, so this query silently matches "+
				"every row. Give the fallback a real predicate before relaxing semantic analysis.", q)
		}
		if !strings.Contains(err.Error(), "parameter as full predicate literal") {
			t.Logf("note: %s is refused with an unexpected message: %v", q, err)
		}
	}

	// The one shape that DOES reach the fallback is an empty map literal, where
	// matching everything is the correct answer rather than a stub's accident.
	res, err := eng.Run(context.Background(), `MATCH (n:P {}) RETURN count(*) AS n`, nil)
	if err != nil {
		t.Fatalf("empty inline map: %v", err)
	}
	var got any
	for res.Next() {
		got = res.Record()["n"]
	}
	if err := res.Err(); err != nil {
		t.Fatalf("empty inline map: %v", err)
	}
	_ = res.Close()
	if fmt.Sprint(got) != "3" {
		t.Fatalf("MATCH (n:P {}) returned %v, want 3 — an empty property map constrains nothing", got)
	}
}
