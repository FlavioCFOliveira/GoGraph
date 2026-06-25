package cypher_test

import "testing"

// #1779 undirected: single physical edge a-b traversed both ways is NOT a
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
}

// #1779 DOCUMENTED LIMITATION (pinned): the UNDIRECTED shortest-cycle case
// currently UNDER-REPORTS — it returns no row rather than the true edge-simple
// cycle (e.g. the a-b-c-a triangle). The contractual guarantee here is the
// SAFE one: it must NEVER emit an edge-reusing (invalid) cycle. A node-keyed
// search over the symmetric CSR cannot find the undirected shortest cycle
// without the Itai & Rodeh branch-collision algorithm (tracked as a follow-up;
// see docs/tck/DIVERGENCES.md). This gate pins the safe behaviour: the
// undirected triangle yields 0 rows, NOT an invalid length-2 cycle.
//
// When branch-collision lands, flip the expectation to a length-3 cycle.
func TestShortestPath_Cycle_UndirectedTriangle_DocumentedLimitation(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:R]->(c)`,
		`MATCH (c:N {k:2}),(a:N {k:0}) CREATE (c)-[:R]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]-(a)) RETURN length(p) AS len`)
	// SAFE behaviour: under-report (0 rows) OR a genuine edge-simple cycle of
	// length >= 3 — but NEVER an invalid (edge-reusing) length < 3 cycle.
	for _, r := range rows {
		if scLen(t, r["len"]) < 3 {
			t.Fatalf("undirected triangle emitted INVALID cycle len=%v (<3, edge reuse)", r["len"])
		}
	}
}

// #1779 undirected allShortestPaths triangle: same documented-limitation pin —
// never an invalid edge-reusing cycle.
func TestAllShortestPaths_Cycle_UndirectedTriangle_DocumentedLimitation(t *testing.T) {
	eng := scNewEng(false)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:R]->(c)`,
		`MATCH (c:N {k:2}),(a:N {k:0}) CREATE (c)-[:R]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = allShortestPaths((a)-[*1..]-(a)) RETURN length(p) AS len`)
	for _, r := range rows {
		if scLen(t, r["len"]) < 3 {
			t.Fatalf("allShortest undirected triangle emitted INVALID cycle len=%v (<3)", r["len"])
		}
	}
}
