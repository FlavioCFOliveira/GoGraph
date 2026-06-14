// Package cypher_ldbc_test — lazy / late node materialisation benchmarks (#1500).
//
// These benchmarks isolate the scalar-projection win: a node bound by MATCH but
// read only through scalar accesses (n.key / n["key"] / n:Label) no longer pays
// to eagerly materialise its full property map and label set per row. The seed
// graph here carries SEVERAL properties per node (unlike the label-only shared
// benchGraph) so that the per-row full-property-map allocation the optimisation
// removes is actually present in the baseline.
//
// The eager-fallback benchmarks (whole-node projection and a whole-node
// accessor in WHERE) exist to prove the optimisation does NOT regress the cases
// that must keep full materialisation.
package cypher_ldbc_test

import (
	"context"
	"fmt"
	"log"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// propSeedSize is the node count for the multi-property lazy benchmark graph.
const propSeedSize = 1000

// propBenchGraph is a read-only graph whose nodes each carry eight properties,
// seeded lazily on first use by lazyPropGraph.
var propBenchGraph *lpg.Graph[string, float64]

// lazyPropGraph builds (once) and returns a directed graph of propSeedSize
// nodes, each labelled Person and carrying eight string/int properties. The
// many-properties-per-node shape is what makes the eager full-property-map copy
// expensive in the baseline, so the scalar-projection win is observable.
func lazyPropGraph() *lpg.Graph[string, float64] {
	if propBenchGraph != nil {
		return propBenchGraph
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < propSeedSize; i++ {
		key := fmt.Sprintf("p%d", i)
		if err := g.AddNode(key); err != nil {
			log.Fatalf("lazy seed AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			log.Fatalf("lazy seed SetNodeLabel: %v", err)
		}
		props := map[string]lpg.PropertyValue{
			"firstName": lpg.StringValue(fmt.Sprintf("first%d", i)),
			"lastName":  lpg.StringValue(fmt.Sprintf("last%d", i)),
			"email":     lpg.StringValue(fmt.Sprintf("user%d@example.com", i)),
			"city":      lpg.StringValue(fmt.Sprintf("city%d", i%50)),
			"country":   lpg.StringValue(fmt.Sprintf("country%d", i%10)),
			"bio":       lpg.StringValue("a moderately long biography string used to give the property map some weight"),
			"age":       lpg.Int64Value(int64(18 + i%60)),
			"score":     lpg.Int64Value(int64(i * 7)),
		}
		for k, v := range props {
			if err := g.SetNodeProperty(key, k, v); err != nil {
				log.Fatalf("lazy seed SetNodeProperty: %v", err)
			}
		}
	}
	g.SetIndexManager(index.NewManager())
	propBenchGraph = g
	return g
}

// runLazyQuery drives query for b.N iterations against the multi-property graph,
// fully draining every result so the projection/filter closures execute per row.
func runLazyQuery(b *testing.B, query string) {
	b.Helper()
	g := lazyPropGraph()
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, query, nil)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		for res.Next() { //nolint:revive // drain every row so the per-row projection/filter closures execute
		}
		if err := res.Err(); err != nil {
			b.Fatalf("drain: %v", err)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}

// BenchmarkLazyScalarProjection is the win target: every node is bound but only
// a single scalar property is projected. Under the optimisation only that one
// property is loaded per row instead of the whole eight-property map.
func BenchmarkLazyScalarProjection(b *testing.B) {
	runLazyQuery(b, "MATCH (n:Person) RETURN n.firstName")
}

// BenchmarkLazyScalarFilterAndProject combines a scalar WHERE predicate with a
// scalar projection — the canonical LDBC IC shape (filter on one property,
// return another).
func BenchmarkLazyScalarFilterAndProject(b *testing.B) {
	runLazyQuery(b, "MATCH (n:Person) WHERE n.age > 40 RETURN n.lastName, n.email")
}

// BenchmarkLazyScalarFilterOnly exercises the WHERE/Filter lazy path on its own
// (projection returns a constant, so the win is entirely in the predicate).
func BenchmarkLazyScalarFilterOnly(b *testing.B) {
	runLazyQuery(b, "MATCH (n:Person) WHERE n.score > 100 RETURN 1")
}

// BenchmarkLazyWholeNodeProjection is the eager-fallback guard: `RETURN n`
// returns the whole node, so full materialisation is required and the lazy path
// must be disabled. This must not regress versus baseline.
func BenchmarkLazyWholeNodeProjection(b *testing.B) {
	runLazyQuery(b, "MATCH (n:Person) RETURN n")
}

// BenchmarkLazyWholeNodeAccessorInWhere is the other eager-fallback guard: a
// whole-node accessor (keys(n)) in the predicate forces needsWholeNode, so the
// node must be fully materialised. This must not regress versus baseline.
func BenchmarkLazyWholeNodeAccessorInWhere(b *testing.B) {
	runLazyQuery(b, "MATCH (n:Person) WHERE size(keys(n)) > 0 RETURN n.firstName")
}
