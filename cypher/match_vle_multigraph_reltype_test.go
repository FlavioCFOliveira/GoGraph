package cypher_test

// match_vle_multigraph_reltype_test.go — rmp #1685 regression: a
// variable-length relationship -[*]- (and the named path it builds) over
// PARALLEL edges with distinct types must report each edge's OWN type and
// properties on a reverse / undirected hop, not a single merged type — the
// multi-hop analogue of the single-hop #1634 / #1684 fixes.
//
// Before the fix, exec.VarLengthExpand recorded a SYNTHETIC reverse edge
// position (base+revPos) that overloaded physical-edge identity and traversal
// direction into one integer the relationship hydrator could not decode, so a
// reverse hop over a parallel multigraph edge collapsed onto the first forward
// slot's type and the coalesced per-pair property union. The fix makes each hop
// carry the handle-disambiguated FORWARD position plus a separate direction
// marker (the stride-3 flat encoding), and routes every VLE / path hop through
// the shared resolveHopRel hydrator, which recovers the edge's stable per-edge
// handle and reports its per-instance type and properties.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// collectSingleHopVLE runs a VLE query that yields one path per parallel edge,
// each path carrying exactly one relationship, and returns the (type, w) pair
// of that single relationship per result row, sorted by type so assertions are
// order-independent. The query MUST project columns named exactly: ts (a
// one-element list of the hop's type) and ws (a one-element list of the hop's
// r.w, possibly Null).
func collectSingleHopVLE(t *testing.T, eng *cypher.Engine, query string) []relRow {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var got []relRow
	for res.Next() {
		rec := res.Record()
		ts, ok := rec["ts"].(expr.ListValue)
		if !ok || len(ts) != 1 {
			t.Fatalf("column ts: expected 1-element ListValue, got %T (%v)", rec["ts"], rec["ts"])
		}
		sv, ok := ts[0].(expr.StringValue)
		if !ok {
			t.Fatalf("column ts[0]: expected StringValue, got %T (%v)", ts[0], ts[0])
		}
		row := relRow{typ: string(sv)}
		if ws, ok := rec["ws"].(expr.ListValue); ok && len(ws) == 1 {
			row.w = ws[0]
		}
		got = append(got, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].typ < got[j].typ })
	return got
}

// TestVLE_Multigraph_ReverseHop_PerInstanceType pins the core #1685 contract: a
// single-hop variable-length relationship over two parallel typed edges reports
// each edge's own type on the forward, reverse and undirected passes. Each
// parallel edge is a distinct relationship under relationship-uniqueness, so the
// VLE yields one path per edge.
func TestVLE_Multigraph_ReverseHop_PerInstanceType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:T1]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:T2]->(b)`,
	)
	want := []string{"T1", "T2"}

	cases := []struct {
		name  string
		query string
	}{
		// Forward (control): was already correct.
		{"forward-rellist", `MATCH (a:N {k:1})-[rs:T1|T2*1..1]->(b:N {k:2}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`},
		// Undirected single hop: from either side, against the edge — the bug.
		{"undirected-rellist", `MATCH (a:N {k:1})-[rs:T1|T2*1..1]-(b:N {k:2}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`},
		// Pure reverse (incoming) single hop.
		{"reverse-rellist", `MATCH (b:N {k:2})<-[rs:T1|T2*1..1]-(a:N {k:1}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`},
		// Named-path form: relationships(p) must surface the same per-instance types.
		{"forward-path", `MATCH p = (a:N {k:1})-[:T1|T2*1..1]->(b:N {k:2}) RETURN [x IN relationships(p) | type(x)] AS ts, [x IN relationships(p) | x.w] AS ws`},
		{"undirected-path", `MATCH p = (a:N {k:1})-[:T1|T2*1..1]-(b:N {k:2}) RETURN [x IN relationships(p) | type(x)] AS ts, [x IN relationships(p) | x.w] AS ws`},
		{"reverse-path", `MATCH p = (b:N {k:2})<-[:T1|T2*1..1]-(a:N {k:1}) RETURN [x IN relationships(p) | type(x)] AS ts, [x IN relationships(p) | x.w] AS ws`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := collectSingleHopVLE(t, eng, c.query)
			if len(rows) != 2 {
				t.Fatalf("%s: got %d paths, want 2 (one per parallel edge)", c.name, len(rows))
			}
			if got := []string{rows[0].typ, rows[1].typ}; !equalStrs(got, want) {
				t.Fatalf("%s: per-hop types = %v, want %v (parallel edges collapsed to a merged type)", c.name, got, want)
			}
		})
	}
}

// TestVLE_Multigraph_PerInstanceProperty pins the property axis of #1685: two
// parallel edges carrying distinct properties must each surface their OWN
// property on a reverse / undirected variable-length hop. A coalesced bug would
// give both paths the same merged w.
func TestVLE_Multigraph_PerInstanceProperty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {w:10}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R2 {w:20}]->(b)`,
	)
	wantW := map[string]int64{"R1": 10, "R2": 20}

	for _, c := range []struct {
		name  string
		query string
	}{
		{"undirected", `MATCH (a:N {k:1})-[rs:R1|R2*1..1]-(b:N {k:2}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`},
		{"reverse", `MATCH (b:N {k:2})<-[rs:R1|R2*1..1]-(a:N {k:1}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := collectSingleHopVLE(t, eng, c.query)
			if len(rows) != 2 {
				t.Fatalf("%s: got %d paths, want 2", c.name, len(rows))
			}
			for _, row := range rows {
				if got := intVal(t, row.w); got != wantW[row.typ] {
					t.Errorf("%s: %s x.w = %d, want %d (parallel-edge property collapsed)", c.name, row.typ, got, wantW[row.typ])
				}
			}
		})
	}
}

// TestVLE_Multigraph_ThreeWayParallel_ReverseHop proves the reverse-hop
// disambiguation is genuinely per-handle and not a "pick first" / "pick last":
// three parallel edges with distinct types+props must each read their own on an
// undirected variable-length hop. A 2-edge test could pass under a first/last
// rule; a 3-edge test cannot.
func TestVLE_Multigraph_ThreeWayParallel_ReverseHop(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {w:10}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R2 {w:20}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R3 {w:30}]->(b)`,
	)
	rows := collectSingleHopVLE(t, eng,
		`MATCH (b:N {k:2})-[rs:R1|R2|R3*1..1]-(a:N {k:1}) RETURN [x IN rs | type(x)] AS ts, [x IN rs | x.w] AS ws`)
	if len(rows) != 3 {
		t.Fatalf("got %d paths, want 3", len(rows))
	}
	want := map[string]int64{"R1": 10, "R2": 20, "R3": 30}
	for _, row := range rows {
		if got := intVal(t, row.w); got != want[row.typ] {
			t.Errorf("%s x.w = %d, want %d (reverse hop not per-handle)", row.typ, got, want[row.typ])
		}
	}
}
