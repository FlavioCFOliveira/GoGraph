package audit352_test

import (
	"context"
	"fmt"
	"log"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relGraph is a smaller fixture whose relationships carry several properties,
// created through Cypher so the relationships land in the by-handle property
// store that a Cypher-created relationship actually uses (the plain Go
// SetEdgeProperty API writes the columnar store instead, which is a different
// read path and would measure the wrong thing).
var relGraph *lpg.Graph[string, float64]

const relNodes = 4000

func buildRelGraph() *lpg.Graph[string, float64] {
	if relGraph != nil {
		return relGraph
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for i := 0; i < relNodes; i++ {
		if _, err := eng.RunAny(context.Background(), fmt.Sprintf(
			`CREATE (:P {sid:%d, nm:'p%d'})`, 100000+i, i), nil); err != nil {
			log.Fatalf("relGraph create node: %v", err)
		}
	}
	// One relationship per node, carrying five properties. Five is chosen to
	// sit below the property bag's 8-record promotion threshold, so the read
	// path under test is the byte-stream bag rather than the promoted map.
	for i := 0; i < relNodes; i++ {
		q := fmt.Sprintf(
			`MATCH (a:P {sid:%d}), (b:P {sid:%d}) CREATE (a)-[:R {w:%d, k1:'v1', k2:'v2', k3:%d, k4:%d}]->(b)`,
			100000+i, 100000+((i+7)%relNodes), 500000+i, 600000+i, 700000+i)
		if _, err := eng.RunAny(context.Background(), q, nil); err != nil {
			log.Fatalf("relGraph create rel: %v", err)
		}
	}
	relGraph = g
	return g
}

// relShapes contrast reading ONE relationship property against reading the
// whole relationship, and against the node-side equivalents. If reading one
// property materialises the entire property map, the one-property arm costs
// close to the whole-relationship arm rather than close to its node analogue.
var relShapes = []struct{ name, query string }{
	{"rel_one_prop", `MATCH ()-[r:R]->() RETURN r.w`},
	{"rel_two_props", `MATCH ()-[r:R]->() RETURN r.w, r.k3`},
	{"rel_whole", `MATCH ()-[r:R]->() RETURN r`},
	{"rel_type_only", `MATCH ()-[r:R]->() RETURN type(r)`},
	{"rel_count", `MATCH ()-[r:R]->() RETURN count(r) AS c`},
	{"node_one_prop", `MATCH (p:P) RETURN p.sid`},
	{"node_whole", `MATCH (p:P) RETURN p`},
}

func BenchmarkRelationshipProps(b *testing.B) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range relShapes {
		s := s
		b.Run(s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}

func TestRelationshipPropsPlans(t *testing.T) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range relShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		t.Logf("--- %s ships %d rows\n%s\n%s", s.name, countRows(t, engine, s.query), s.query, p)
	}
}

// subqueryShapes compare a CALL subquery against the OPTIONAL MATCH that
// computes the same thing, plus their non-subquery baselines. Both arms must
// ship the same rows; TestSubqueryShapeRowCounts asserts it.
var subqueryShapes = []struct{ name, query string }{
	{"plain_match", `MATCH (a:P) RETURN a.sid`},
	{"optional_match", `MATCH (a:P) OPTIONAL MATCH (a)-[:R]->(b:P) RETURN a.sid, b.sid`},
	{"exists_subquery", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:R]->(:P) } RETURN a.sid`},
	{"count_subquery", `MATCH (a:P) RETURN a.sid AS sid, COUNT { MATCH (a)-[:R]->(:P) } AS c`},
	{"pattern_predicate", `MATCH (a:P) WHERE (a)-[:R]->(:P) RETURN a.sid`},
}

func BenchmarkSubqueryShape(b *testing.B) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range subqueryShapes {
		s := s
		b.Run(s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}

func TestSubqueryShapeRowCounts(t *testing.T) {
	g := buildRelGraph()
	engine := cypher.NewEngine(g)
	for _, s := range subqueryShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		t.Logf("--- %s ships %d rows\n%s\n%s", s.name, countRows(t, engine, s.query), s.query, p)
	}
}
