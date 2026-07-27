//go:build r4audit

package r4audit

import (
	"fmt"
	"testing"
)

// TestSharedVariableJoinPlans maps how the translator plans a multi-pattern
// MATCH whose patterns share a node variable — the commonest join in graph
// querying, which Neo4j serves with NodeHashJoin or a bound-side Expand.
func TestSharedVariableJoinPlans(t *testing.T) {
	eng := newEng(t, 200)
	shapes := []struct{ name, q string }{
		{"shared-dest-comma", `MATCH (a:P)-[:K]->(b:P), (c:P)-[:K]->(b) RETURN count(*)`},
		{"shared-dest-2match", `MATCH (a:P)-[:K]->(b:P) MATCH (c:P)-[:K]->(b) RETURN count(*)`},
		{"shared-src-comma", `MATCH (a:P)-[:K]->(b:P), (a)-[:K]->(c:P) RETURN count(*)`},
		{"chain-comma", `MATCH (a:P)-[:K]->(b:P), (b)-[:K]->(c:P) RETURN count(*)`},
		{"chain-inline", `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN count(*)`},
		{"triangle-inline", `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*)`},
		{"prop-equijoin", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)`},
		{"shared-dest-where", `MATCH (a:P)-[:K]->(b:P), (c:P)-[:K]->(d:P) WHERE b = d RETURN count(*)`},
	}
	for _, s := range shapes {
		plan, err := eng.Explain(s.q, nil)
		if err != nil {
			fmt.Printf("=== %-20s ERROR %v\n", s.name, err)
			continue
		}
		fmt.Printf("=== %s\n%s\n", s.name, plan)
	}
}
