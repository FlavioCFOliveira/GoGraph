package cypher_test

// shortest_cycle_test.go — regression gate for #1779: shortestPath/
// allShortestPaths with src == dst and a lower hop bound >= 1 must find the
// shortest non-trivial cycle back to the source, honouring relationship-
// uniqueness. The zero-length *0.. self path must keep returning length 0.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func scNewEng(directed bool) *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: directed})
	return cypher.NewEngine(g)
}

func scSeed(t *testing.T, eng *cypher.Engine, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := eng.RunInTx(context.Background(), q, nil); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

func scRows(t *testing.T, eng *cypher.Engine, q string) []map[string]expr.Value {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer res.Close()
	var out []map[string]expr.Value
	for res.Next() {
		rec := res.Record()
		cp := map[string]expr.Value{}
		for k, v := range rec {
			if ev, ok := v.(expr.Value); ok {
				cp[k] = ev
			}
		}
		out = append(out, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", q, err)
	}
	return out
}

func scLen(t *testing.T, v expr.Value) int64 {
	t.Helper()
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("expected IntegerValue, got %T (%v)", v, v)
	}
	return int64(iv)
}

// kCycle seeds a directed k-cycle 0->1->...->(k-1)->0.
func kCycle(t *testing.T, eng *cypher.Engine, k int) {
	t.Helper()
	for i := 0; i < k; i++ {
		scSeed(t, eng, "CREATE (:N {k:"+itoaSC(i)+"})")
	}
	for i := 0; i < k; i++ {
		j := (i + 1) % k
		scSeed(t, eng, "MATCH (a:N {k:"+itoaSC(i)+"}),(b:N {k:"+itoaSC(j)+"}) CREATE (a)-[:R]->(b)")
	}
}

func itoaSC(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestShortestPath_Cycle_DirectedK asserts shortestPath((a)-[*1..]->(a)) over a
// directed k-cycle returns one row of length k. Pre-fix this returns 0 rows.
func TestShortestPath_Cycle_DirectedK(t *testing.T) {
	for _, k := range []int{3, 4, 5} {
		eng := scNewEng(true)
		kCycle(t, eng, k)
		rows := scRows(t, eng,
			`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]->(a)) RETURN length(p) AS len`)
		if len(rows) != 1 {
			t.Fatalf("k=%d: got %d rows, want 1", k, len(rows))
		}
		if got := scLen(t, rows[0]["len"]); got != int64(k) {
			t.Errorf("k=%d: length = %d, want %d", k, got, k)
		}
	}
}

// TestShortestPath_Cycle_BoundedWindow asserts the *1..5 form finds the len-3
// cycle (pre-fix: 0 rows).
func TestShortestPath_Cycle_BoundedWindow(t *testing.T) {
	eng := scNewEng(true)
	kCycle(t, eng, 3)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..5]->(a)) RETURN length(p) AS len`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 3 {
		t.Fatalf("*1..5 cycle: got %v, want 1 row len=3", dumpLens(rows))
	}
}

// TestShortestPath_Cycle_ZeroLowerBoundStillZero asserts the *0.. self path is
// unchanged: length 0 (must NOT regress to the cycle).
func TestShortestPath_Cycle_ZeroLowerBoundStillZero(t *testing.T) {
	eng := scNewEng(true)
	kCycle(t, eng, 3)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*0..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 0 {
		t.Fatalf("*0.. self: got %v, want 1 row len=0", dumpLens(rows))
	}
}

// TestShortestPath_Cycle_SelfLoop asserts a self-loop is the length-1 shortest
// cycle.
func TestShortestPath_Cycle_SelfLoop(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`,
		`MATCH (a:N {k:0}) CREATE (a)-[:R]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 1 {
		t.Fatalf("self-loop: got %v, want 1 row len=1", dumpLens(rows))
	}
}

// TestShortestPath_Cycle_NoCycleNoRow asserts that with no cycle, MATCH yields
// no row (and OPTIONAL MATCH yields one null row).
func TestShortestPath_Cycle_NoCycle(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`, // a->b only, no way back
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 0 {
		t.Fatalf("no-cycle MATCH: got %v, want 0 rows", dumpLens(rows))
	}
	orows := scRows(t, eng,
		`MATCH (a:N {k:0}) OPTIONAL MATCH p = shortestPath((a)-[*1..]->(a)) RETURN p AS p`)
	if len(orows) != 1 {
		t.Fatalf("no-cycle OPTIONAL: got %d rows, want 1", len(orows))
	}
	if v := orows[0]["p"]; !expr.IsNull(v) {
		t.Errorf("no-cycle OPTIONAL: p = %v, want null", v)
	}
}

// TestShortestPath_Cycle_TwoCycleDistinctEdges asserts a genuine directed
// 2-cycle (two distinct arcs a->b, b->a) is a valid length-2 cycle.
func TestShortestPath_Cycle_TwoCycleDistinctEdges(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(a:N {k:0}) CREATE (b)-[:R]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = shortestPath((a)-[*1..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 1 || scLen(t, rows[0]["len"]) != 2 {
		t.Fatalf("directed 2-cycle: got %v, want 1 row len=2", dumpLens(rows))
	}
}

// TestAllShortestPaths_Cycle asserts allShortestPaths over a single directed
// k-cycle returns exactly one cycle of length k.
func TestAllShortestPaths_Cycle_Single(t *testing.T) {
	for _, k := range []int{3, 4} {
		eng := scNewEng(true)
		kCycle(t, eng, k)
		rows := scRows(t, eng,
			`MATCH (a:N {k:0}) MATCH p = allShortestPaths((a)-[*1..]->(a)) RETURN length(p) AS len`)
		if len(rows) != 1 {
			t.Fatalf("k=%d allShortest: got %d rows, want 1", k, len(rows))
		}
		if got := scLen(t, rows[0]["len"]); got != int64(k) {
			t.Errorf("k=%d allShortest: length = %d, want %d", k, got, k)
		}
	}
}

// TestAllShortestPaths_Cycle_TwoTied asserts two tied shortest cycles through
// src are both returned. Graph: 0->1->0 and 0->2->0 (two distinct directed
// 2-cycles). Expect 2 cycles, each length 2.
func TestAllShortestPaths_Cycle_TwoTied(t *testing.T) {
	eng := scNewEng(true)
	scSeed(t, eng,
		`CREATE (a:N {k:0})`, `CREATE (b:N {k:1})`, `CREATE (c:N {k:2})`,
		`MATCH (a:N {k:0}),(b:N {k:1}) CREATE (a)-[:R]->(b)`,
		`MATCH (b:N {k:1}),(a:N {k:0}) CREATE (b)-[:R]->(a)`,
		`MATCH (a:N {k:0}),(c:N {k:2}) CREATE (a)-[:R]->(c)`,
		`MATCH (c:N {k:2}),(a:N {k:0}) CREATE (c)-[:R]->(a)`,
	)
	rows := scRows(t, eng,
		`MATCH (a:N {k:0}) MATCH p = allShortestPaths((a)-[*1..]->(a)) RETURN length(p) AS len`)
	if len(rows) != 2 {
		t.Fatalf("two tied 2-cycles: got %d rows, want 2 (%v)", len(rows), dumpLens(rows))
	}
	for _, r := range rows {
		if scLen(t, r["len"]) != 2 {
			t.Errorf("two tied 2-cycles: length = %d, want 2", scLen(t, r["len"]))
		}
	}
}

func dumpLens(rows []map[string]expr.Value) []string {
	var out []string
	for _, r := range rows {
		out = append(out, sprintfV(r["len"]))
	}
	return out
}

func sprintfV(v expr.Value) string {
	if v == nil {
		return "<nil>"
	}
	return v.String()
}
