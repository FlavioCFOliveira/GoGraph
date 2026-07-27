//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
)

// TestSubqueryForms maps exactly which EXISTS/COUNT/COLLECT subquery forms the
// parser accepts, against what openCypher 2024.3 and Neo4j 5 accept.
func TestSubqueryForms(t *testing.T) {
	eng := newEng(t, 50)
	forms := []struct{ name, q string }{
		// EXISTS
		{"exists-pattern-only", `MATCH (a:P) WHERE EXISTS { (a)-[:K]->(:P) } RETURN count(a)`},
		{"exists-match-nowhere", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(:P) } RETURN count(a)`},
		{"exists-match-where", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b:P) WHERE b.id > 1 } RETURN count(a)`},
		{"exists-match-return", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b:P) RETURN b } RETURN count(a)`},
		{"exists-old-func", `MATCH (a:P) WHERE exists(a.name) RETURN count(a)`},
		{"exists-union", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b) RETURN b UNION MATCH (a)<-[:K]-(b) RETURN b } RETURN count(a)`},
		{"exists-with", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b) WITH b WHERE b.id > 1 RETURN b } RETURN count(a)`},
		// COUNT
		{"count-pattern-only", `MATCH (a:P) WHERE COUNT { (a)-[:K]->(:P) } > 0 RETURN count(a)`},
		{"count-match-nowhere", `MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(:P) } > 0 RETURN count(a)`},
		{"count-match-return", `MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(b) RETURN b } > 0 RETURN count(a)`},
		{"count-in-return", `MATCH (a:P) RETURN a.id, COUNT { (a)-[:K]->() } AS deg LIMIT 3`},
		// COLLECT
		{"collect-pattern", `MATCH (a:P) RETURN COLLECT { MATCH (a)-[:K]->(b) RETURN b.id } AS ids LIMIT 3`},
		// CALL subquery
		{"call-subquery-with", `MATCH (a:P) CALL { WITH a MATCH (a)-[:K]->(b) RETURN b } RETURN count(b)`},
		{"call-subquery-import", `CALL { MATCH (n:P) RETURN n LIMIT 1 } RETURN n`},
		// pattern predicates (the pre-subquery spelling)
		{"pattern-pred-pos", `MATCH (a:P) WHERE (a)-[:K]->(:P) RETURN count(a)`},
		{"pattern-pred-neg", `MATCH (a:P) WHERE NOT (a)-[:K]->(:P) RETURN count(a)`},
	}
	for _, f := range forms {
		res, err := eng.RunAny(context.Background(), f.q, nil)
		switch {
		case err != nil:
			fmt.Printf("%-24s REJECT  %v\n", f.name, err)
		default:
			n := 0
			for res.Next() {
				n++
			}
			if e := res.Err(); e != nil {
				fmt.Printf("%-24s EVALERR %v\n", f.name, e)
				continue
			}
			fmt.Printf("%-24s ACCEPT  rows=%d\n", f.name, n)
		}
	}
}
