package cypher_test

import "testing"

// shortest_cycle_undirected_test.go — regression gate for #1785: the UNDIRECTED
// shortest-cycle case of shortestPath/allShortestPaths with src == dst.
//
// An undirected edge is stored as TWO forward-CSR arcs that share ONE stable
// handle, so the closing edge {m,s} can be the SAME relationship as the
// shortest-prefix edge {s,m}. A node-keyed single-predecessor BFS therefore
// MISSES the true edge-simple cycle (the a-b-c-a triangle needs a non-shortest
// prefix). These tests pin the Itai & Rodeh (1978) branch-collision result:
// the triangle yields the length-3 edge-simple cycle with the CORRECT per-hop
// relationship types, a single undirected edge yields no length-2 reuse, and
// allShortestPaths returns both traversal orientations.

// #1785 undirected: single physical edge a-b traversed both ways is NOT a
// 2-cycle (relationship-uniqueness). No other path back -> 0 rows.
func TestShortestPath_Cycle_UndirectedSingleEdgeNoReuse(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]-(a)) RETURN length(p) AS len`)
	if len(rows) != 0 {
		t.Fatalf("undirected single edge: got %v, want 0 rows (no edge reuse)", dumpLens(rows))
	}
	// allShortestPaths over the same single edge: also no cycle.
	arows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = allShortestPaths((a)-[*1..]-(a)) RETURN length(p) AS len`)
	if len(arows) != 0 {
		t.Fatalf("allShortest single edge: got %v, want 0 rows", dumpLens(arows))
	}
}

// #1785 undirected triangle: shortestPath returns the ONE length-3 edge-simple
// cycle a-b-c-a, with the correct per-hop relationship type in order. The three
// edges carry DISTINCT types so a wrong/merged hydration is caught.
func TestShortestPath_Cycle_UndirectedTriangle(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:AB]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:BC]->(c)`,
		`MATCH (c:N {k:2}),(a:N {k:0}) CREATE (c)-[:CA]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]-(a))
		 RETURN length(p) AS len, [r IN relationships(p) | type(r)] AS types`)
	if len(rows) != 1 {
		t.Fatalf("undirected triangle shortestPath: got %d rows, want 1", len(rows))
	}
	if got := scLen(t, rows[0]["len"]); got != 3 {
		t.Fatalf("undirected triangle: len = %d, want 3 (edge-simple cycle)", got)
	}
	types := scStringList(t, rows[0]["types"])
	if len(types) != 3 {
		t.Fatalf("undirected triangle: %d hops, want 3 (per-hop types %v)", len(types), types)
	}
	// The cycle is one of the two orientations a->b->c->a or a->c->b->a; both
	// are edge-simple and carry all three distinct types exactly once.
	if !scTypeSetEqual(types, []string{"AB", "BC", "CA"}) {
		t.Fatalf("undirected triangle per-hop types = %v, want a permutation of [AB BC CA] "+
			"(an empty type means reversed-arm hydration is wrong)", types)
	}
}

// #1785 undirected triangle allShortestPaths: BOTH traversal orientations of
// the one undirected triangle are returned as distinct length-3 paths
// (a Cypher path is orientation-ordered even over an undirected pattern), each
// with the correct per-hop types in order.
func TestAllShortestPaths_Cycle_UndirectedTriangle(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:AB]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:BC]->(c)`,
		`MATCH (c:N {k:2}),(a:N {k:0}) CREATE (c)-[:CA]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = allShortestPaths((a)-[*1..]-(a))
		 RETURN length(p) AS len, [r IN relationships(p) | type(r)] AS types, [n IN nodes(p) | n.k] AS ks`)
	if len(rows) != 2 {
		t.Fatalf("undirected triangle allShortestPaths: got %d rows, want 2 (both orientations)", len(rows))
	}
	for _, r := range rows {
		if got := scLen(t, r["len"]); got != 3 {
			t.Fatalf("undirected triangle: len = %d, want 3", got)
		}
		types := scStringList(t, r["types"])
		if len(types) != 3 || !scTypeSetEqual(types, []string{"AB", "BC", "CA"}) {
			t.Fatalf("undirected triangle per-hop types = %v, want a permutation of [AB BC CA]", types)
		}
	}
	// The two rows must be the two opposite orientations, distinguished by the
	// node ordering a->b->c->a vs a->c->b->a.
	o1 := scIntList(t, rows[0]["ks"])
	o2 := scIntList(t, rows[1]["ks"])
	if scIntSliceEqual(o1, o2) {
		t.Fatalf("allShortestPaths returned the same orientation twice (ks=%v), want both orientations", o1)
	}
	want1 := []int64{0, 1, 2, 0}
	want2 := []int64{0, 2, 1, 0}
	got := map[string]bool{scIntKey(o1): true, scIntKey(o2): true}
	if !got[scIntKey(want1)] || !got[scIntKey(want2)] {
		t.Fatalf("allShortestPaths orientations = %v / %v, want %v and %v", o1, o2, want1, want2)
	}
}

// #1785 undirected self-loop: a self-edge {a,a} is a valid length-1 cycle.
func TestShortestPath_Cycle_UndirectedSelfLoop(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`,
		`MATCH (a:N {k:0}) CREATE (a)-[:SELF]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]-(a))
		 RETURN length(p) AS len, [r IN relationships(p) | type(r)] AS types`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 1 {
		t.Fatalf("undirected self-loop: got %v, want 1 row len=1", dumpLens(rows))
	}
	if types := scStringList(t, rows[0]["types"]); len(types) != 1 || types[0] != "SELF" {
		t.Fatalf("undirected self-loop type = %v, want [SELF]", types)
	}
}
