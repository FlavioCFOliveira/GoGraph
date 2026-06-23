package cypher_test

// match_shortest_path_test.go — rmp #1692 integration tests for
// shortestPath()/allShortestPaths() execution.
//
// These exercise the full pipeline (parser normaliser → sema → IR → physical
// builder → exec operators → per-instance hydration) over a live LPG graph,
// including the multigraph parallel-typed-edge case the dedicated operators now
// resolve through the shared resolveHopRel hydrator (the same path the VLE uses).

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runShortest runs a single query and returns every result record.
func runShortest(t *testing.T, eng *cypher.Engine, query string) []exec.Record {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var out []exec.Record
	for res.Next() {
		rec := res.Record()
		cp := make(exec.Record, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		out = append(out, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	return out
}

// recIsNull reports whether a record column holds a null value.
func recIsNull(v interface{}) bool {
	ev, ok := v.(expr.Value)
	return ok && expr.IsNull(ev)
}

// TestShortestPath_BasicChain checks a single shortest path over a simple
// directed chain a→b→c→d.
func TestShortestPath_BasicChain(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
		`CREATE (c:N {k:2})`,
		`CREATE (d:N {k:3})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:R]->(c)`,
		`MATCH (c:N {k:2}),(d:N {k:3}) CREATE (c)-[:R]->(d)`,
	)

	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(d:N {k:3}) MATCH p = shortestPath((a)-[*]->(d))
		 RETURN length(p) AS len, [r IN relationships(p) | type(r)] AS types`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if lv := rows[0]["len"]; lv != expr.IntegerValue(3) {
		t.Errorf("length = %v, want 3", lv)
	}
	types := stringList(t, rows[0]["types"])
	if len(types) != 3 || types[0] != "R" || types[1] != "R" || types[2] != "R" {
		t.Errorf("types = %v, want [R R R]", types)
	}
}

// TestShortestPath_NoPath_Match verifies that an unreachable pair under MATCH
// eliminates the row.
func TestShortestPath_NoPath_Match(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
	)
	// No edge between a and b → directed shortestPath has no path.
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(b:N {k:1}) MATCH p = shortestPath((a)-[*]->(b)) RETURN p`)
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (no path under MATCH)", len(rows))
	}
}

// TestShortestPath_NoPath_OptionalMatch verifies that an unreachable pair under
// OPTIONAL MATCH keeps the row with p = null.
func TestShortestPath_NoPath_OptionalMatch(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
	)
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(b:N {k:1}) OPTIONAL MATCH p = shortestPath((a)-[*]->(b)) RETURN p`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (null path under OPTIONAL MATCH)", len(rows))
	}
	if !recIsNull(rows[0]["p"]) {
		t.Errorf("p = %v, want null", rows[0]["p"])
	}
}

// TestAllShortestPaths_Ties checks that allShortestPaths returns every
// minimum-length path over a diamond a→b→d, a→c→d.
func TestAllShortestPaths_Ties(t *testing.T) {
	// Diamond a→b→d, a→c→d (no direct a→d edge): two length-2 shortest paths.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
		`CREATE (c:N {k:2})`,
		`CREATE (d:N {k:3})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (a:N {k:0}),(c:N {k:2}) CREATE (a)-[:R]->(c)`,
		`MATCH (b:N {k:1}),(d:N {k:3}) CREATE (b)-[:R]->(d)`,
		`MATCH (c:N {k:2}),(d:N {k:3}) CREATE (c)-[:R]->(d)`,
	)
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(d:N {k:3}) MATCH p = allShortestPaths((a)-[*]->(d))
		 RETURN length(p) AS len`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (two length-2 shortest paths)", len(rows))
	}
	for i, r := range rows {
		if r["len"] != expr.IntegerValue(2) {
			t.Errorf("rows[%d] len = %v, want 2", i, r["len"])
		}
	}
}

// TestShortestPath_Undirected checks an undirected shortestPath traverses
// against storage direction.
func TestShortestPath_Undirected(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
		`CREATE (c:N {k:2})`,
		// Stored a→b, c→b. Undirected path a—b—c exists; directed a→…→c does not.
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (c:N {k:2}),(b:N {k:1}) CREATE (c)-[:R]->(b)`,
	)
	// Directed: no path.
	if rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]->(c)) RETURN p`); len(rows) != 0 {
		t.Fatalf("directed: got %d rows, want 0", len(rows))
	}
	// Undirected: a—b—c, length 2.
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[*]-(c)) RETURN length(p) AS len`)
	if len(rows) != 1 {
		t.Fatalf("undirected: got %d rows, want 1", len(rows))
	}
	if rows[0]["len"] != expr.IntegerValue(2) {
		t.Errorf("undirected length = %v, want 2", rows[0]["len"])
	}
}

// TestShortestPath_TypeFilter checks the relationship-type disjunction filter.
func TestShortestPath_TypeFilter(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:0})`,
		`CREATE (b:N {k:1})`,
		`CREATE (c:N {k:2})`,
		// Short path a→c is type X (excluded). Long path a→b→c is type T.
		`MATCH (a:N {k:0}),(c:N {k:2}) CREATE (a)-[:X]->(c)`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:T]->(b)`,
		`MATCH (b:N {k:1}),(c:N {k:2}) CREATE (b)-[:T]->(c)`,
	)
	// Filtering to T only: the only path is a→b→c (length 2); the length-1 X
	// edge is excluded.
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}),(c:N {k:2}) MATCH p = shortestPath((a)-[:T*]->(c))
		 RETURN length(p) AS len, [r IN relationships(p) | type(r)] AS types`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["len"] != expr.IntegerValue(2) {
		t.Errorf("length = %v, want 2 (X edge excluded)", rows[0]["len"])
	}
	types := stringList(t, rows[0]["types"])
	for _, ty := range types {
		if ty != "T" {
			t.Errorf("type = %q, want only T", ty)
		}
	}
}

// TestShortestPath_MultigraphPerInstanceType is the core rmp #1692 contract: a
// shortest path crossing a multigraph pair with PARALLEL typed edges reports
// the OWN type of the edge actually traversed, not a merged type. Because both
// parallel edges connect the same pair, the shortest path uses one of them; the
// reported type must be one of the real types (T1 or T2), and the property read
// must track it.
func TestShortestPath_MultigraphPerInstanceType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N {k:1}),(b:N {k:2}) CREATE (a)-[:T1 {w:10}]->(b)`,
		`MATCH (a:N {k:1}),(b:N {k:2}) CREATE (a)-[:T2 {w:20}]->(b)`,
	)
	// allShortestPaths must enumerate BOTH parallel length-1 edges, each with
	// its own type and matching property.
	rows := runShortest(t, eng,
		`MATCH (a:N {k:1}),(b:N {k:2}) MATCH p = allShortestPaths((a)-[*]->(b))
		 RETURN [r IN relationships(p) | type(r)] AS types, [r IN relationships(p) | r.w] AS ws`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (two parallel length-1 edges)", len(rows))
	}
	type pair struct {
		typ string
		w   int64
	}
	got := make([]pair, 0, len(rows))
	for _, r := range rows {
		types := stringList(t, r["types"])
		if len(types) != 1 {
			t.Fatalf("path types = %v, want exactly one hop", types)
		}
		ws, ok := r["ws"].(expr.ListValue)
		if !ok || len(ws) != 1 {
			t.Fatalf("ws = %v, want 1-element list", r["ws"])
		}
		wv, ok := ws[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("ws[0] = %T, want IntegerValue", ws[0])
		}
		got = append(got, pair{typ: types[0], w: int64(wv)})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].typ < got[j].typ })
	want := []pair{{"T1", 10}, {"T2", 20}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %+v, want %+v (per-instance type/property must track)", i, got[i], want[i])
		}
	}
}

// TestShortestPath_ZeroLength checks src == dst with a 0 lower bound yields a
// zero-length path.
func TestShortestPath_ZeroLength(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng, `CREATE (a:N {k:0})`)
	rows := runShortest(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*0..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["len"] != expr.IntegerValue(0) {
		t.Errorf("zero-length path length = %v, want 0", rows[0]["len"])
	}
}
