package cypher_test

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestRepro_NegatedPatternPredicate_NonFirstRelType probes the reported defect
// across every pattern-predicate form with two parallel edges of DIFFERENT
// types between the same pair:
//
//	(a)-[:FIRST]->(b)   (a)-[:SECOND]->(b)
func TestRepro_NegatedPatternPredicate_NonFirstRelType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (a:N {name:'a'})-[:FIRST]->(b:N {name:'b'})`)
	runSetup(t, eng, `MATCH (a:N {name:'a'}), (b:N {name:'b'}) CREATE (a)-[:SECOND]->(b)`)

	type probe struct {
		q    string
		want int // number of 'name' rows expected
	}
	probes := []probe{
		// MATCH baseline.
		{`MATCH (a:N {name:'a'})-[:FIRST]->() RETURN a.name AS name`, 1},
		{`MATCH (a:N {name:'a'})-[:SECOND]->() RETURN a.name AS name`, 1},
		// Bare pattern predicate, positive — outgoing.
		{`MATCH (a:N {name:'a'}) WHERE (a)-[:FIRST]->() RETURN a.name AS name`, 1},
		{`MATCH (a:N {name:'a'}) WHERE (a)-[:SECOND]->() RETURN a.name AS name`, 1},
		// Bare pattern predicate, negated — outgoing.
		{`MATCH (a:N {name:'a'}) WHERE NOT (a)-[:FIRST]->() RETURN a.name AS name`, 0},
		{`MATCH (a:N {name:'a'}) WHERE NOT (a)-[:SECOND]->() RETURN a.name AS name`, 0},
		// Incoming direction from the destination side.
		{`MATCH (b:N {name:'b'}) WHERE (b)<-[:SECOND]-() RETURN b.name AS name`, 1},
		{`MATCH (b:N {name:'b'}) WHERE NOT (b)<-[:SECOND]-() RETURN b.name AS name`, 0},
		// Undirected.
		{`MATCH (a:N {name:'a'}) WHERE (a)-[:SECOND]-() RETURN a.name AS name`, 1},
		// Variable-length with the non-first type.
		{`MATCH (a:N {name:'a'}) WHERE (a)-[:SECOND*1..2]->() RETURN a.name AS name`, 1},
		// Multi-type predicate ordering both ways.
		{`MATCH (a:N {name:'a'}) WHERE (a)-[:SECOND|FIRST]->() RETURN a.name AS name`, 1},
		// EXISTS subquery form (SemiApply/Expand path — already correct).
		{`MATCH (a:N {name:'a'}) WHERE EXISTS { (a)-[:SECOND]->() } RETURN a.name AS name`, 1},
		{`MATCH (a:N {name:'a'}) WHERE NOT EXISTS { (a)-[:SECOND]->() } RETURN a.name AS name`, 0},
	}
	for _, p := range probes {
		names := collectNameCol(t, eng, p.q)
		if len(names) != p.want {
			t.Errorf("got %v (n=%d), want n=%d\n  query: %s", names, len(names), p.want, p.q)
		}
	}
}

// TestRepro_PatternComprehension_RelVarType probes the sibling defect: a
// relationship variable bound inside a pattern comprehension over a
// multigraph pair should report the matched edge's OWN type, not the first
// label of the pair. `type(r)` for `[r:SECOND]` must yield "SECOND".
func TestRepro_PatternComprehension_RelVarType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (a:N {name:'a'})-[:FIRST]->(b:N {name:'b'})`)
	runSetup(t, eng, `MATCH (a:N {name:'a'}), (b:N {name:'b'}) CREATE (a)-[:SECOND]->(b)`)

	collectStrList := func(q string) []string {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", q, err)
		}
		rows := collectRecords(t, res)
		if len(rows) != 1 {
			t.Fatalf("%q: got %d rows, want 1", q, len(rows))
		}
		lv, ok := rows[0]["ts"].(expr.ListValue)
		if !ok {
			t.Fatalf("%q: ts column is %T, want ListValue", q, rows[0]["ts"])
		}
		out := make([]string, 0, len(lv))
		for _, v := range lv {
			if sv, ok := v.(expr.StringValue); ok {
				out = append(out, string(sv))
			}
		}
		sort.Strings(out)
		return out
	}

	// [r:SECOND] restricts to the SECOND edge; type(r) must be "SECOND".
	if got := collectStrList(`MATCH (a:N {name:'a'}) RETURN [(a)-[r:SECOND]->() | type(r)] AS ts`); len(got) != 1 || got[0] != "SECOND" {
		t.Errorf("comprehension [r:SECOND] type(r): got %v, want [SECOND]", got)
	}
	// [r:FIRST] restricts to the FIRST edge; type(r) must be "FIRST".
	if got := collectStrList(`MATCH (a:N {name:'a'}) RETURN [(a)-[r:FIRST]->() | type(r)] AS ts`); len(got) != 1 || got[0] != "FIRST" {
		t.Errorf("comprehension [r:FIRST] type(r): got %v, want [FIRST]", got)
	}
}

// TestRepro_ComprehensionFallback_RelVarType covers the sibling defect on the
// EvalPatternComp fallback path (a comprehension nested inside size() in a
// WHERE clause), where a bound relationship variable's type(r) previously came
// from EdgeLabels(pair)[0] — the first-stored label — instead of the type the
// pattern selected. Both parallel edges exist, so filtering the comprehension
// by either type must retain 'a'.
func TestRepro_ComprehensionFallback_RelVarType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (a:N {name:'a'})-[:FIRST]->(b:N {name:'b'})`)
	runSetup(t, eng, `MATCH (a:N {name:'a'}), (b:N {name:'b'}) CREATE (a)-[:SECOND]->(b)`)

	for _, rt := range []string{"FIRST", "SECOND"} {
		q := `MATCH (a:N {name:'a'}) WHERE size([(a)-[r:` + rt +
			`]->() WHERE type(r) = '` + rt + `' | 1]) > 0 RETURN a.name AS name`
		names := collectNameCol(t, eng, q)
		if len(names) != 1 || names[0] != "a" {
			t.Errorf("comprehension-fallback type(r)=%s: got %v, want [a]", rt, names)
		}
	}
}
