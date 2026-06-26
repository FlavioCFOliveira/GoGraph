package cypher_test

// shortest_path_where_test.go — regression gate for #1786: a whole-path
// predicate that references the path variable is evaluated DURING the
// shortestPath()/allShortestPaths() search (an exhaustive fallback), so the
// operator returns the shortest path that SATISFIES the predicate rather than
// the unconstrained shortest path post-filtered away.
//
// In Neo4j a predicate that inspects the whole matched path (e.g. length(p) > 1)
// triggers a slower, exhaustive search returning the shortest satisfying path.
// Previously GoGraph applied such a predicate as a post-filter above the
// operator, so a shorter path that fails the predicate dropped the row even when
// a longer satisfying path existed.

import "testing"

// Graph: a->b->c plus a direct a->c (length-1) edge. Unconstrained shortest
// a->c is the direct edge (length 1); the length-2 path a->b->c also exists.
func TestShortestPath_WherePathPredicate_ExhaustiveFallback(t *testing.T) {
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

	// WHERE length(p) > 1 references the path variable: the operator searches
	// exhaustively and returns the shortest SATISFYING path, length 2 (a->b->c).
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) WHERE length(p) > 1 RETURN length(p) AS len`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 2 {
		t.Fatalf("WHERE length(p)>1: got %v, want 1 row len=2 (exhaustive fallback)", dumpLens(rows))
	}

	// A predicate the unconstrained shortest path already satisfies returns it
	// unchanged (length 1).
	ok := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) WHERE length(p) >= 1 RETURN length(p) AS len`)
	if len(ok) != 1 || scLen(t, ok[0]["len"]) != 1 {
		t.Fatalf("WHERE length(p)>=1: got %v, want 1 row len=1", dumpLens(ok))
	}

	// An unsatisfiable predicate yields no row under MATCH.
	none := scRows(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) WHERE length(p) > 5 RETURN length(p) AS len`)
	if len(none) != 0 {
		t.Fatalf("WHERE length(p)>5 (unsatisfiable): got %v, want 0 rows", dumpLens(none))
	}
}

// allShortestPaths with a whole-path predicate returns ALL shortest SATISFYING
// paths (the minimum satisfying length). Diamond a->b->d, a->c->d plus a direct
// a->d: unconstrained shortest is the direct length-1 edge; WHERE length(p) > 1
// must return BOTH length-2 paths a->b->d and a->c->d.
func TestAllShortestPaths_WherePathPredicate_ExhaustiveFallback(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`, `CREATE (d:N {k:3})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (a:N {k:0}),(c:N {k:2}) CREATE (a)-[:R]->(c)`,
		`MATCH (b:N {k:1}),(d:N {k:3}) CREATE (b)-[:R]->(d)`,
		`MATCH (c:N {k:2}),(d:N {k:3}) CREATE (c)-[:R]->(d)`,
		`MATCH (a:N {k:0}),(d:N {k:3}) CREATE (a)-[:R]->(d)`,
	)

	// Unconstrained: the direct a->d edge, length 1.
	base := scRows(t, eng,
		`MATCH (a:N {k:0}),(d:N {k:3}) MATCH p = allShortestPaths((a)-[*]->(d)) RETURN length(p) AS len`)
	if len(base) != 1 || scLen(t, base[0]["len"]) != 1 {
		t.Fatalf("no-predicate allShortest: got %v, want 1 row len=1", dumpLens(base))
	}

	// WHERE length(p) > 1: both length-2 paths satisfy and are the shortest
	// satisfying length.
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}),(d:N {k:3}) MATCH p = allShortestPaths((a)-[*]->(d)) WHERE length(p) > 1 RETURN length(p) AS len`)
	if len(rows) != 2 {
		t.Fatalf("allShortest WHERE length(p)>1: got %d rows, want 2 (both length-2 paths)", len(rows))
	}
	for _, r := range rows {
		if scLen(t, r["len"]) != 2 {
			t.Fatalf("allShortest WHERE length(p)>1: len = %d, want 2", scLen(t, r["len"]))
		}
	}
}
