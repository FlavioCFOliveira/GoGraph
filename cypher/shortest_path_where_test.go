package cypher_test

// shortest_path_where_test.go — pins the CURRENT (documented-divergence)
// behaviour of a whole-path predicate fused onto shortestPath() (#1782).
//
// In Neo4j a predicate that inspects the whole matched path (e.g. length(p) > 1)
// triggers an exhaustive search returning the shortest path that SATISFIES the
// predicate. GoGraph currently applies such a predicate as a post-filter above
// the operator, so a shorter path that fails the predicate is dropped even when
// a longer satisfying path exists. This test pins that behaviour so a future
// implementation of the exhaustive fallback is a deliberate, visible change.
// See docs/tck/DIVERGENCES.md.

import "testing"

// Graph: a->b->c plus a direct a->c (length-1) edge. Unconstrained shortest
// a->c is the direct edge (length 1).
func TestShortestPath_WherePostFilter_CurrentBehaviour(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:R]->(c)`,
		`MATCH (a:N {k:0}),(c:N {k:2}) CREATE (a)-[:R]->(c)`,
	)

	// No predicate: shortest is the direct edge, length 1.
	base := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) RETURN length(p) AS len`)
	if len(base) != 1 || scLen(t, base[0]["len"]) != 1 {
		t.Fatalf("no-predicate shortest: got %v, want 1 row len=1", dumpLens(base))
	}

	// CURRENT behaviour (documented divergence): WHERE length(p) > 1 is a
	// post-filter on the single unconstrained shortest path (length 1), which
	// fails the predicate -> 0 rows. Neo4j would return the length-2 path.
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) WHERE length(p) > 1 RETURN length(p) AS len`)
	if len(rows) != 0 {
		// If a future change implements the exhaustive fallback, this becomes
		// 1 row len=2; update the divergence doc and this assertion together.
		t.Fatalf("WHERE length(p)>1 post-filter: got %v, want 0 rows (current behaviour); "+
			"if this now returns len=2, the exhaustive-fallback fix landed — update docs/tck/DIVERGENCES.md", dumpLens(rows))
	}

	// A predicate the unconstrained shortest path satisfies is returned normally.
	ok := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) WHERE length(p) >= 1 RETURN length(p) AS len`)
	if len(ok) != 1 || scLen(t, ok[0]["len"]) != 1 {
		t.Fatalf("WHERE length(p)>=1: got %v, want 1 row len=1", dumpLens(ok))
	}
}
