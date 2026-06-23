package cypher_test

// match_multigraph_reltype_test.go — rmp #1634 regression: an undirected
// (or reverse) hop over PARALLEL edges with distinct types must report
// each edge's own type, not a single merged type.
//
// Before the fix, exec.Expand.tryRevEdge mapped every reverse edge of a
// (dst -> src) pair onto the FIRST forward-CSR position via
// lookupFwdEdgePos, so two parallel edges A-[:T1]->B and A-[:T2]->B both
// resolved to T1 on the reverse hop. The fix disambiguates by the stable
// per-instance handle that csr.BuildReverse carries on each reverse slot.

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func collectRelTypes(t *testing.T, eng *cypher.Engine, query string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var got []string
	for res.Next() {
		rec := res.Record()
		sv, ok := rec["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("column t: expected StringValue, got %T (%v)", rec["t"], rec["t"])
		}
		got = append(got, string(sv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Strings(got)
	return got
}

// TestMatch_Multigraph_ReverseHop_PerInstanceType pins the #1634 contract:
// the forward hop and the undirected reverse hop over two parallel,
// distinctly-typed edges both yield {T1, T2}.
func TestMatch_Multigraph_ReverseHop_PerInstanceType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	seed := []string{
		`CREATE (a:A {id: 1})`,
		`CREATE (b:B {id: 2})`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:T1]->(b)`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:T2]->(b)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}

	want := []string{"T1", "T2"}

	// Forward hop (control): was already correct.
	if got := collectRelTypes(t, eng, `MATCH (a:A)-[r]->(b:B) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("forward hop types = %v, want %v", got, want)
	}
	// Undirected reverse hop (the bug): from the :B side, against the edge.
	if got := collectRelTypes(t, eng, `MATCH (b:B)-[r]-(a:A) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("undirected reverse-hop types = %v, want %v (parallel edges collapsed to a merged type)", got, want)
	}
	// Pure reverse hop (incoming): same contract.
	if got := collectRelTypes(t, eng, `MATCH (b:B)<-[r]-(a:A) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("reverse incoming-hop types = %v, want %v", got, want)
	}
}

// TestMatch_Multigraph_ParallelSelfLoops_Undirected pins that two
// parallel typed self-loops report distinct per-instance types on an
// undirected match. Self-loops are emitted by the forward pass and
// deduplicated on the reverse pass (tryRevEdge skips dst == srcID), so
// this exercises the forward per-instance path rather than the reverse
// fix — it must stay correct.
//
// NOT covered here (tracked as a separate same-family defect, rmp #1684):
// OPPOSITE-direction parallel edges (a)-[:T1]->(b) and (b)-[:T2]->(a)
// still collapse type(r) on an undirected hop, because the storage
// direction probe in buildRelationshipValueFromRow cannot tell which
// stored direction an emitted edge came from when both directions exist.
func TestMatch_Multigraph_ParallelSelfLoops_Undirected(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seed := []string{
		`CREATE (a:A {id: 1})`,
		`MATCH (a:A) WHERE a.id = 1 CREATE (a)-[:T1]->(a)`,
		`MATCH (a:A) WHERE a.id = 1 CREATE (a)-[:T2]->(a)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}
	if got := collectRelTypes(t, eng, `MATCH (n:A)-[r]-(m:A) RETURN type(r) AS t`); !equalStrs(got, []string{"T1", "T2"}) {
		t.Fatalf("undirected parallel self-loops types = %v, want [T1 T2]", got)
	}
}

// TestMerge_Multigraph_DistinctType_CreatesParallelEdge is the #1683
// regression: MERGE of a second, distinctly-typed relationship between an
// existing pair must CREATE the parallel edge (not bind to the first
// edge), and each must report its own type. Before the fix, MERGE's match
// used a type-agnostic HasEdge, so MERGE (a)-[:T2]->(b) bound to the T1
// edge and no T2 edge was created.
func TestMerge_Multigraph_DistinctType_CreatesParallelEdge(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seed := []string{
		`CREATE (a:A {id: 1})`,
		`CREATE (b:B {id: 2})`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T1]->(b)`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T2]->(b)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}
	// Two distinctly-typed parallel edges, each reporting its own type.
	if got := collectRelTypes(t, eng, `MATCH (a:A)-[r]->(b:B) RETURN type(r) AS t`); !equalStrs(got, []string{"T1", "T2"}) {
		t.Fatalf("after two distinct-type MERGEs, types = %v, want [T1 T2]", got)
	}
	// Idempotency: re-MERGE an existing type must NOT create a third edge.
	res, err := eng.RunAny(ctx, `MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T1]->(b)`, nil)
	if err != nil {
		t.Fatalf("idempotent MERGE: %v", err)
	}
	for res.Next() {
	}
	res.Close()
	if got := collectRelTypes(t, eng, `MATCH (a:A)-[r]->(b:B) RETURN type(r) AS t`); !equalStrs(got, []string{"T1", "T2"}) {
		t.Fatalf("after idempotent re-MERGE of T1, types = %v, want [T1 T2] (a third edge was created)", got)
	}
}

// TestMerge_Multigraph_Undirected_DistinctType_CreatesParallelEdge covers
// the undirected MERGE probe (the reverse-direction match also gained the
// type check): two distinct-type undirected MERGEs create two edges, and
// re-MERGE of an existing type is idempotent.
func TestMerge_Multigraph_Undirected_DistinctType_CreatesParallelEdge(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	stmts := []string{
		`CREATE (a:A {id: 1})`,
		`CREATE (b:B {id: 2})`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T1]-(b)`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T2]-(b)`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 MERGE (a)-[:T1]-(b)`, // idempotent
	}
	for _, q := range stmts {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("stmt %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain %q: %v", q, err)
		}
		res.Close()
	}
	// Undirected MERGE stores a single direction; read it back directed.
	if got := collectRelTypes(t, eng, `MATCH (a:A)-[r]->(b:B) RETURN type(r) AS t`); !equalStrs(got, []string{"T1", "T2"}) {
		t.Fatalf("undirected distinct-type MERGEs (+ idempotent re-MERGE) types = %v, want [T1 T2]", got)
	}
}

func equalStrs(a, b []string) bool {
	return slices.Equal(a, b)
}

// ─────────────────────────────────────────────────────────────────────────────
// rmp #1684 — per-instance PROPERTIES on multigraph parallel edges.
//
// Before the fix, buildRelationshipValueFromRow resolved a bound relationship's
// TYPE per-instance (by stable handle, #1634) but its PROPERTIES per-PAIR: the
// whole-r / properties(r) / r.prop read routed through the latest-wins coalesced
// union over every parallel slot, so two parallel edges
// (a)-[:R1 {w:10}]->(b) and (a)-[:R2 {w:20}]->(b) both reported w=20 (and a
// merged property map). The fix routes the property read through the SAME
// per-handle store the type path uses, so each bound r reports its OWN edge's
// properties — congruent with the already-per-instance type(r). It reproduces
// with PURE CREATE (no MERGE). Scope: single-hop emit + scalar / whole-r property
// reads; VLE and shortestPath path rendering are the sibling ticket #1685.
// ─────────────────────────────────────────────────────────────────────────────

// seedMultigraph runs each statement to completion against eng, failing the test
// on any error. It is the shared CREATE/MERGE seeder for the #1684 scenarios.
func seedMultigraph(t *testing.T, eng *cypher.Engine, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range stmts {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}
}

// relRow is one MATCH result row, projecting the bound relationship's type
// alongside per-instance property facts, so a test can assert that the property
// reads track the type rather than collapsing to a pair-coalesced union.
type relRow struct {
	typ      string
	w        interface{} // r.w (scalar read), an exec.Record column value; nil => Null
	keys     []string    // sorted keys(r)
	wNotNull bool        // r.w IS NOT NULL
}

// collectRelRows runs query and returns one relRow per result row, sorted by
// type so the assertions are order-independent. The query MUST project columns
// named exactly: t (type(r)), w (r.w), ks (keys(r)), wnn (r.w IS NOT NULL).
func collectRelRows(t *testing.T, eng *cypher.Engine, query string) []relRow {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var got []relRow
	for res.Next() {
		rec := res.Record()
		sv, ok := rec["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("column t: expected StringValue, got %T (%v)", rec["t"], rec["t"])
		}
		row := relRow{typ: string(sv), w: rec["w"]}
		if lv, ok := rec["ks"].(expr.ListValue); ok {
			for _, e := range lv {
				if s, ok := e.(expr.StringValue); ok {
					row.keys = append(row.keys, string(s))
				}
			}
			sort.Strings(row.keys)
		}
		if b, ok := rec["wnn"].(expr.BoolValue); ok {
			row.wNotNull = bool(b)
		}
		got = append(got, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].typ < got[j].typ })
	return got
}

// intVal asserts the exec.Record column value v is an expr.IntegerValue and
// returns it; -1 sentinel on a nil / Null column so an assertion can distinguish
// "absent" from a real value.
func intVal(t *testing.T, v interface{}) int64 {
	t.Helper()
	if v == nil {
		return -1
	}
	if ev, ok := v.(expr.Value); ok && expr.IsNull(ev) {
		return -1
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("expected IntegerValue, got %T (%v)", v, v)
	}
	return int64(iv)
}

// TestMatch_Multigraph_PerInstanceProperties is the core #1684 regression: two
// parallel edges of distinct types AND distinct property values AND distinct key
// SETS, created by pure CREATE, read back per-instance for the bound r on every
// single-hop direction — forward, undirected, and opposite-direction.
func TestMatch_Multigraph_PerInstanceProperties(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	// Distinct types, distinct w, and distinct key sets: R1 carries {w}, R2
	// carries {w, extra}. keys(r) must therefore diverge per row and
	// `r.extra IS NULL` must be true on R1's row, false on R2's.
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {w:10}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R2 {w:20, extra:99}]->(b)`,
	)

	// Each hop direction must surface BOTH edges, each with its own type+props.
	dirs := []struct {
		name  string
		query string
	}{
		{"forward", `MATCH (a:N {k:1})-[r]->(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`},
		{"undirected", `MATCH (a:N {k:1})-[r]-(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`},
		{"opposite", `MATCH (b:N {k:2})<-[r]-(a:N {k:1}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`},
		{"opposite-undirected", `MATCH (b:N {k:2})-[r]-(a:N {k:1}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`},
	}
	for _, d := range dirs {
		t.Run(d.name, func(t *testing.T) {
			rows := collectRelRows(t, eng, d.query)
			if len(rows) != 2 {
				t.Fatalf("%s: got %d rows, want 2 (one per parallel edge)", d.name, len(rows))
			}
			// rows are sorted by type: [R1, R2].
			r1, r2 := rows[0], rows[1]
			if r1.typ != "R1" || r2.typ != "R2" {
				t.Fatalf("%s: types = [%s %s], want [R1 R2]", d.name, r1.typ, r2.typ)
			}
			// r.w is per-instance, not a coalesced 20-on-both.
			if got := intVal(t, r1.w); got != 10 {
				t.Errorf("%s: R1 r.w = %d, want 10 (parallel-edge property collapsed)", d.name, got)
			}
			if got := intVal(t, r2.w); got != 20 {
				t.Errorf("%s: R2 r.w = %d, want 20", d.name, got)
			}
			// keys(r) is per-instance: R1 has {w}, R2 has {extra, w}.
			if !equalStrs(r1.keys, []string{"w"}) {
				t.Errorf("%s: R1 keys(r) = %v, want [w]", d.name, r1.keys)
			}
			if !equalStrs(r2.keys, []string{"extra", "w"}) {
				t.Errorf("%s: R2 keys(r) = %v, want [extra w]", d.name, r2.keys)
			}
			// r.w IS NOT NULL is true on both (both carry w).
			if !r1.wNotNull || !r2.wNotNull {
				t.Errorf("%s: r.w IS NOT NULL = [%v %v], want [true true]", d.name, r1.wNotNull, r2.wNotNull)
			}
		})
	}

	// properties(r) / RETURN r whole-map render is per-instance too: the bound r
	// projects its OWN map, not the merged {w,extra} union on both rows.
	t.Run("properties-whole-map", func(t *testing.T) {
		res, err := eng.RunAny(context.Background(),
			`MATCH (a:N {k:1})-[r]-(b:N {k:2}) RETURN type(r) AS t, properties(r) AS p`, nil)
		if err != nil {
			t.Fatalf("properties query: %v", err)
		}
		defer res.Close()
		byType := map[string]expr.MapValue{}
		for res.Next() {
			rec := res.Record()
			sv := rec["t"].(expr.StringValue)
			mv, ok := rec["p"].(expr.MapValue)
			if !ok {
				t.Fatalf("column p: expected MapValue, got %T (%v)", rec["p"], rec["p"])
			}
			byType[string(sv)] = mv
		}
		if err := res.Err(); err != nil {
			t.Fatalf("properties iteration: %v", err)
		}
		// R1: exactly {w:10}. R2: exactly {w:20, extra:99}. A coalesced bug would
		// give both the same merged map.
		if got := intVal(t, byType["R1"]["w"]); got != 10 || len(byType["R1"]) != 1 {
			t.Errorf("properties(R1) = %v, want {w:10}", byType["R1"])
		}
		if got := intVal(t, byType["R2"]["w"]); got != 20 {
			t.Errorf("properties(R2)[w] = %d, want 20", got)
		}
		if got := intVal(t, byType["R2"]["extra"]); got != 99 || len(byType["R2"]) != 2 {
			t.Errorf("properties(R2) = %v, want {w:20, extra:99}", byType["R2"])
		}
		// extra is absent from R1's own map (per-instance), so r.extra IS NULL there.
		if _, present := byType["R1"]["extra"]; present {
			t.Errorf("properties(R1) leaked R2's 'extra' key: %v", byType["R1"])
		}
	})
}

// TestMatch_Multigraph_ThreeWayParallel_PerInstanceProperty proves the read is
// genuinely per-HANDLE and not a subtly-wrong "pick first" / "pick last": three
// parallel edges with three distinct w values must each read their own. A 2-edge
// test could pass under a first/last rule; a 3-edge test cannot.
func TestMatch_Multigraph_ThreeWayParallel_PerInstanceProperty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {w:10}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R2 {w:20}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R3 {w:30}]->(b)`,
	)
	rows := collectRelRows(t, eng,
		`MATCH (a:N {k:1})-[r]-(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	want := map[string]int64{"R1": 10, "R2": 20, "R3": 30}
	for _, row := range rows {
		if got := intVal(t, row.w); got != want[row.typ] {
			t.Errorf("%s r.w = %d, want %d (not per-handle)", row.typ, got, want[row.typ])
		}
	}
}

// TestMatch_Multigraph_ParallelSelfLoops_PerInstanceProperty pins the self-loop
// axis: two parallel typed self-loops (a)-[:R1{w:10}]->(a) and
// (a)-[:R2{w:20}]->(a) must each read their own w on an undirected hop. Self-loops
// are emitted by the forward pass and deduplicated on the reverse pass, so this
// exercises the forward per-instance property path.
func TestMatch_Multigraph_ParallelSelfLoops_PerInstanceProperty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`MATCH (a:N) WHERE a.k = 1 CREATE (a)-[:R1 {w:10}]->(a)`,
		`MATCH (a:N) WHERE a.k = 1 CREATE (a)-[:R2 {w:20}]->(a)`,
	)
	rows := collectRelRows(t, eng,
		`MATCH (n:N {k:1})-[r]-(m:N {k:1}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if got := intVal(t, rows[0].w); got != 10 {
		t.Errorf("self-loop R1 r.w = %d, want 10", got)
	}
	if got := intVal(t, rows[1].w); got != 20 {
		t.Errorf("self-loop R2 r.w = %d, want 20", got)
	}
}

// TestMatch_Multigraph_MixedHandleSentinel_PerInstanceProperty exercises the
// fallback boundary MID-PAIR: between the same (a,b) one edge is created by
// CREATE (real handle) and one by MERGE (carries the 0 handle sentinel on the
// older write path). The real-handle edge must read per-instance while the
// sentinel edge falls back to the per-pair coalesced map without corrupting the
// other row's read. Both rows must still surface, each with its own type.
func TestMatch_Multigraph_MixedHandleSentinel_PerInstanceProperty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {w:10}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 MERGE (a)-[:R2 {w:20}]->(b)`,
	)
	rows := collectRelRows(t, eng,
		`MATCH (a:N {k:1})-[r]-(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].typ != "R1" || rows[1].typ != "R2" {
		t.Fatalf("types = [%s %s], want [R1 R2]", rows[0].typ, rows[1].typ)
	}
	// The real-handle CREATE edge reads its own w; the read must not be polluted
	// by the sibling. (The MERGE edge's exact w depends on the handle it carries;
	// the contract under test is that R1 stays correct and both rows surface.)
	if got := intVal(t, rows[0].w); got != 10 {
		t.Errorf("mixed: R1 (real handle) r.w = %d, want 10", got)
	}
}

// TestMatch_NonMultigraph_SingleEdge_PropertyUnchanged is the fwdHandle==0
// fallback: a single non-parallel edge in a SIMPLE (non-multigraph) graph reads
// its property exactly as before. This guards that the per-handle routing does
// not disturb the overwhelming common case.
func TestMatch_NonMultigraph_SingleEdge_PropertyUnchanged(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})-[:R {w:42}]->(b:N {k:2})`,
	)
	for _, q := range []string{
		`MATCH (a:N {k:1})-[r]->(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`,
		`MATCH (a:N {k:1})-[r]-(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`,
		`MATCH (b:N {k:2})<-[r]-(a:N {k:1}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`,
	} {
		rows := collectRelRows(t, eng, q)
		if len(rows) != 1 {
			t.Fatalf("query %q: got %d rows, want 1", q, len(rows))
		}
		if rows[0].typ != "R" {
			t.Errorf("query %q: type = %s, want R", q, rows[0].typ)
		}
		if got := intVal(t, rows[0].w); got != 42 {
			t.Errorf("query %q: r.w = %d, want 42", q, got)
		}
		if !equalStrs(rows[0].keys, []string{"w"}) {
			t.Errorf("query %q: keys(r) = %v, want [w]", q, rows[0].keys)
		}
		if !rows[0].wNotNull {
			t.Errorf("query %q: r.w IS NOT NULL = false, want true", q)
		}
	}
}

// scalarValue runs a single-column, single-row query and returns that column's
// value, failing the test on any error or on a row count other than one.
func scalarValue(t *testing.T, eng *cypher.Engine, query, col string) interface{} {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var (
		val   interface{}
		count int
	)
	for res.Next() {
		val = res.Record()[col]
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	if count != 1 {
		t.Fatalf("query %q: got %d rows, want 1", query, count)
	}
	return val
}

// TestMatch_Multigraph_PerInstanceProperty_AfterSet is the proof that landing
// the by-handle SET/REMOVE maintenance (#1686) BEFORE this read-routing (#1684)
// was the correct sequencing. Before #1686 the by-handle property store was
// written only at CREATE, so routing the read by-handle returned the STALE
// CREATE-time value after any SET; that is why #1684's read-routing was reverted
// until #1686 made the by-handle store SET-maintained.
//
// Three parallel edges between the SAME pair carry num 1, 2, 3. `SET r.num =
// r.num + 1` over the three bound rows increments each instance's OWN num to 2,
// 3, 4 (cypher-expert-confirmed openCypher semantics: per-row update against the
// distinct relationship entity, RHS reading that entity's prior value — NOT a
// per-pair coalesced write). The post-SET read MUST then surface 2, 3, 4
// per-instance and aggregate to sum 9.
//
// This mirrors the shape of openCypher TCK Set6 [20] ("Aggregating in RETURN
// after setting a property on relationships", sum = 20 over FIVE DISJOINT
// single-edge pairs) but on PARALLEL edges of one pair, which the TCK does not
// cover — a single-edge pair makes per-pair and per-handle storage physically
// indistinguishable, so only this parallel variant witnesses the regression.
func TestMatch_Multigraph_PerInstanceProperty_AfterSet(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R1 {num:1}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R2 {num:2}]->(b)`,
		`MATCH (a:N), (b:N) WHERE a.k = 1 AND b.k = 2 CREATE (a)-[:R3 {num:3}]->(b)`,
	)

	// SET r.num = r.num + 1 over the three parallel instances. Each row reads its
	// own prior num and writes its own +1: 1,2,3 -> 2,3,4.
	seedMultigraph(t, eng,
		`MATCH (a:N {k:1})-[r]->(b:N {k:2}) SET r.num = r.num + 1`,
	)

	// Per-instance read-back: each parallel relationship reports its OWN post-SET
	// num, never a sibling's. A pre-#1686 stale-by-handle read would have returned
	// the CREATE-time 1,2,3 here; a per-pair-coalesced read would return 4 (the
	// latest write) on every row.
	want := map[string]int64{"R1": 2, "R2": 3, "R3": 4}
	rows := collectRelRows(t, eng,
		`MATCH (a:N {k:1})-[r]->(b:N {k:2}) RETURN type(r) AS t, r.num AS w, keys(r) AS ks, r.num IS NOT NULL AS wnn`)
	if len(rows) != 3 {
		t.Fatalf("after SET: got %d rows, want 3 (one per parallel edge)", len(rows))
	}
	for _, row := range rows {
		if got := intVal(t, row.w); got != want[row.typ] {
			t.Errorf("after SET: %s r.num = %d, want %d (stale by-handle or coalesced read)", row.typ, got, want[row.typ])
		}
		if !equalStrs(row.keys, []string{"num"}) {
			t.Errorf("after SET: %s keys(r) = %v, want [num]", row.typ, row.keys)
		}
		if !row.wNotNull {
			t.Errorf("after SET: %s r.num IS NOT NULL = false, want true", row.typ)
		}
	}

	// Aggregating after SET (the Set6 [20] shape, on parallel edges): sum = 2+3+4 = 9.
	if got := intVal(t, scalarValue(t, eng,
		`MATCH (a:N {k:1})-[r]->(b:N {k:2}) RETURN sum(r.num) AS s`, "s")); got != 9 {
		t.Errorf("after SET: sum(r.num) = %d, want 9 (2+3+4)", got)
	}
}

// TestMatch_Merge_OnMatchSet_PerInstanceProperty pins the MERGE ON MATCH SET
// by-handle dual-write that the read-routing of #1684 required. Before the fix,
// MERGE's ON MATCH / ON CREATE action path (applyRelActions) wrote relationship
// property mutations to the per-pair store only, so an edge CREATEd with a
// stable handle (which seeds the by-handle store) then mutated via ON MATCH SET
// left the by-handle store stale — and the by-handle read returned the
// CREATE-time value. It is the same shape as openCypher TCK Merge7 [4]/[5]
// ("Copying properties ... with ON MATCH"), which the by-handle routing broke
// until applyRelActions learnt to mirror its writes by-handle.
//
// This is a SINGLE edge (non-parallel), so it exercises the convergence
// invariant the TCK pins (by-handle == per-pair for a one-edge pair) on the
// MERGE-action write path specifically; the TCK scenarios use a single key, so
// this adds a multi-key SET r = node case to widen the guard.
func TestMatch_Merge_OnMatchSet_PerInstanceProperty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:A {name:'A', tag:'keep'})`,
		`CREATE (b:B {name:'B'})`,
		// CREATE seeds the by-handle store with the edge's own props.
		`MATCH (a:A), (b:B) CREATE (a)-[:TYPE {name:'bar'}]->(b)`,
		// ON MATCH SET r = a copies a's props onto the matched edge (per-pair),
		// and must mirror onto the same edge's by-handle store (#1684).
		`MATCH (a:A), (b:B) MERGE (a)-[r:TYPE]->(b) ON MATCH SET r = a`,
	)
	// The read routes by-handle; it must report a's copied props, not the stale
	// CREATE-time {name:'bar'}. SET r = node is overwrite-only (it does not clear
	// keys absent from a), so name:'bar' -> name:'A' and tag:'keep' is added.
	rows := collectRelRows(t, eng,
		`MATCH (a:A)-[r:TYPE]->(b:B) RETURN type(r) AS t, r.name AS w, keys(r) AS ks, r.name IS NOT NULL AS wnn`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// r.name is the copied 'A' (a StringValue, not the int-shaped helper); read it
	// directly to assert the per-instance value.
	name := scalarValue(t, eng, `MATCH (a:A)-[r:TYPE]->(b:B) RETURN r.name AS n`, "n")
	if sv, ok := name.(expr.StringValue); !ok || string(sv) != "A" {
		t.Errorf("ON MATCH SET r = a: r.name = %v, want \"A\" (stale by-handle 'bar' = the bug)", name)
	}
	if !equalStrs(rows[0].keys, []string{"name", "tag"}) {
		t.Errorf("ON MATCH SET r = a: keys(r) = %v, want [name tag]", rows[0].keys)
	}
	if !rows[0].wNotNull {
		t.Errorf("ON MATCH SET r = a: r.name IS NOT NULL = false, want true")
	}
}

// TestMatch_Merge_OnCreateSet_ParallelEdge_TargetsNewEdge guards the multigraph
// ON CREATE handle resolution: MERGE creates a NEW parallel edge when a sibling
// of a DIFFERENT type already exists, and its ON CREATE SET must write to the
// just-created edge's handle, not the pre-existing sibling's first slot. The fix
// passes the AddEdgeH handle directly to applyRelActions on the create path
// (FirstEdgeHandle would resolve the older sibling's slot).
func TestMatch_Merge_OnCreateSet_ParallelEdge_TargetsNewEdge(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (a:N {k:1})`,
		`CREATE (b:N {k:2})`,
		// Pre-existing parallel sibling of a DIFFERENT type, carrying its own prop.
		`MATCH (a:N {k:1}), (b:N {k:2}) CREATE (a)-[:R1 {w:10}]->(b)`,
		// MERGE of a distinct type creates a new parallel edge; ON CREATE sets w
		// on THAT new edge — it must not land on R1's slot.
		`MATCH (a:N {k:1}), (b:N {k:2}) MERGE (a)-[r:R2]->(b) ON CREATE SET r.w = 20`,
	)
	rows := collectRelRows(t, eng,
		`MATCH (a:N {k:1})-[r]->(b:N {k:2}) RETURN type(r) AS t, r.w AS w, keys(r) AS ks, r.w IS NOT NULL AS wnn`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per parallel edge)", len(rows))
	}
	// Each edge reports its OWN w: R1 keeps 10, the MERGE-created R2 gets 20.
	if got := intVal(t, rows[0].w); got != 10 {
		t.Errorf("R1 r.w = %d, want 10 (ON CREATE leaked onto sibling)", got)
	}
	if got := intVal(t, rows[1].w); got != 20 {
		t.Errorf("R2 r.w = %d, want 20 (ON CREATE missed the new edge)", got)
	}
}

// TestMatch_GoAPIEdge_PropertiesReadThroughPerPair guards the by-handle
// MEMBERSHIP signal (rmp #1684). An edge created through the public Go API
// (Graph.AddEdge/AddEdgeH then Graph.SetEdgeProperty) gets a non-zero stable
// handle stamped on its slot, but its properties go to the per-pair store ONLY —
// the by-handle store has NO entry for that handle. Routing such an edge's
// property reads by-handle would return an empty map and drop every property
// (the regression a realistic Go-API example surfaced: a relationship with
// {state, role} read back with neither). The read path must detect the absent
// by-handle entry and fall back to the per-pair store.
func TestMatch_GoAPIEdge_PropertiesReadThroughPerPair(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	// Build the edge purely through the Go API — no Cypher CREATE, so nothing
	// writes the by-handle store, exactly like a consumer wiring a graph directly.
	if err := g.AddNode("d"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("tk"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddEdgeH("d", "tk", 1.0); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("d", "tk", "ASSIGNED_TO")
	if err := g.SetEdgeProperty("d", "tk", "state", lpg.StringValue("planned")); err != nil {
		t.Fatal(err)
	}
	if err := g.SetEdgeProperty("d", "tk", "role", lpg.StringValue("dev")); err != nil {
		t.Fatal(err)
	}

	eng := cypher.NewEngine(g)
	// WHERE on a rel property + RETURN of another rel property: both must read
	// the per-pair store (the only place the Go-API edge's properties live).
	rows := collectRelRows(t, eng,
		`MATCH (d)-[r:ASSIGNED_TO]->(tk) WHERE r.state <> 'done' RETURN type(r) AS t, r.role AS w, keys(r) AS ks, r.state IS NOT NULL AS wnn`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (Go-API edge's properties dropped)", len(rows))
	}
	if rows[0].typ != "ASSIGNED_TO" {
		t.Errorf("type(r) = %s, want ASSIGNED_TO", rows[0].typ)
	}
	if sv, ok := rows[0].w.(expr.StringValue); !ok || string(sv) != "dev" {
		t.Errorf("r.role = %v, want \"dev\" (empty by-handle store leaked through)", rows[0].w)
	}
	if !equalStrs(rows[0].keys, []string{"role", "state"}) {
		t.Errorf("keys(r) = %v, want [role state]", rows[0].keys)
	}
	if !rows[0].wNotNull {
		t.Errorf("r.state IS NOT NULL = false, want true")
	}
}

// TestMatch_Multigraph_ZeroPropParallelSibling pins the membership signal's
// hardest case (rmp #1684): a Cypher CREATE of two parallel edges of the SAME
// type where one carries a property and the other carries NONE. The
// zero-property instance has an EMPTY by-handle property bag but a NON-EMPTY
// by-handle TYPE entry, so it must still route by-handle and report keys(r)=[] /
// r.k IS NULL — it must NOT fall back to the per-pair coalesced store and leak
// the propertied sibling's key. (A naive "by-handle only when the property bag
// is non-empty" predicate would leak; the type-entry membership marker prevents
// it.)
func TestMatch_Multigraph_ZeroPropParallelSibling(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	seedMultigraph(t, eng,
		`CREATE (x:X)`,
		`CREATE (y:Y)`,
		`MATCH (x:X), (y:Y) CREATE (x)-[:R {k:1}]->(y)`,
		`MATCH (x:X), (y:Y) CREATE (x)-[:R]->(y)`, // zero-prop sibling, same type
	)
	// Two rows: the propertied one (k=1, keys=[k]) and the zero-prop one
	// (k=null, keys=[]). Collected and asserted order-independently.
	res, err := eng.RunAny(context.Background(),
		`MATCH (x:X)-[r:R]->(y:Y) RETURN r.k AS k, keys(r) AS ks, r.k IS NULL AS knull`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	var withK, withoutK int
	for res.Next() {
		rec := res.Record()
		ks := 0
		if lv, ok := rec["ks"].(expr.ListValue); ok {
			ks = len(lv)
		}
		kNull, _ := rec["knull"].(expr.BoolValue)
		switch {
		case intVal(t, rec["k"]) == 1 && ks == 1 && !bool(kNull):
			withK++
		case intVal(t, rec["k"]) == -1 && ks == 0 && bool(kNull):
			withoutK++
		default:
			t.Errorf("unexpected row: k=%v keys-len=%d k-is-null=%v", rec["k"], ks, bool(kNull))
		}
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	if withK != 1 || withoutK != 1 {
		t.Errorf("rows: propertied=%d zero-prop=%d, want 1 and 1 (sibling key leaked)", withK, withoutK)
	}
}
