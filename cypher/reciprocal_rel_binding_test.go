package cypher_test

// reciprocal_rel_binding_test.go — rmp #2504 regression: a RECIPROCAL pair
// (edges in BOTH directions between the same node pair) made every attribute
// read on a reverse or undirected hop resolve to the OTHER edge of the pair.
//
// id(r) was right throughout — it is the stable per-edge handle since
// ef633fe3 — while r.tag, startNode(r) and endNode(r) reported the reciprocal
// edge's data. Non-reciprocal edges were correct in every direction, which is
// why the whole TCK suite and every existing fixture missed it.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// reciprocalSeed builds the #2504 fixture: three Persons A, B, C and three
// KNOWS edges created in this order —
//
//	A->B {tag:'AB'}  (id 1)
//	B->A {tag:'BA'}  (id 2)
//	B->C {tag:'BC'}  (id 3)
//
// A->B / B->A form the reciprocal pair that triggers the defect; B->C is the
// non-reciprocal control that was correct before the fix and must stay correct.
var reciprocalSeed = []string{
	`CREATE (:Person {name:'A'})`,
	`CREATE (:Person {name:'B'})`,
	`CREATE (:Person {name:'C'})`,
	`MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) CREATE (a)-[:KNOWS {tag:'AB'}]->(b)`,
	`MATCH (a:Person {name:'B'}), (b:Person {name:'A'}) CREATE (a)-[:KNOWS {tag:'BA'}]->(b)`,
	`MATCH (a:Person {name:'B'}), (b:Person {name:'C'}) CREATE (a)-[:KNOWS {tag:'BC'}]->(b)`,
}

// runSeed executes every statement in seed against eng, failing the test on the
// first error.
func runSeed(t *testing.T, eng *cypher.Engine, seed []string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { // drain the writer so the statement commits
		}
		if err := res.Err(); err != nil {
			res.Close()
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}
}

// collectRowStrings runs query and renders every row as the space-joined
// rendering of cols, in the order given, so a row can be compared as one string.
func collectRowStrings(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			if sv, ok := rec[c].(expr.StringValue); ok {
				parts = append(parts, string(sv))
				continue
			}
			parts = append(parts, fmt.Sprintf("%v", rec[c]))
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	return out
}

// assertRows compares got against want element-by-element, in order.
func assertRows(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: row count = %d, want %d\n  got  = %v\n  want = %v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: row %d = %q, want %q", label, i, got[i], want[i])
		}
	}
}

// newReciprocalEngine builds the in-memory multigraph fixture.
func newReciprocalEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSeed(t, eng, reciprocalSeed)
	return eng
}

const reciprocalProjection = `RETURN a.name AS an, b.name AS bn, id(r) AS rid, r.tag AS tag, ` +
	`startNode(r).name AS sn, endNode(r).name AS en ORDER BY id(r)`

// TestReciprocalRelBinding_Forward is the CONTROL: the forward pattern was
// already correct at HEAD and must stay byte-identical after the fix.
func TestReciprocalRelBinding_Forward(t *testing.T) {
	eng := newReciprocalEngine(t)
	got := collectRowStrings(t, eng,
		`MATCH (a:Person)-[r:KNOWS]->(b:Person) `+reciprocalProjection,
		"an", "bn", "rid", "tag", "sn", "en")
	assertRows(t, "forward", got, []string{
		"A B 1 AB A B",
		"B A 2 BA B A",
		"B C 3 BC B C",
	})
}

// TestReciprocalRelBinding_Reverse is the RED case. The binding a=A,b=B selects
// edge id 1 (A->B, tag AB); before the fix every attribute read reported edge
// 2's data (tag BA, startNode B, endNode A) while id(r) stayed correct.
func TestReciprocalRelBinding_Reverse(t *testing.T) {
	eng := newReciprocalEngine(t)
	got := collectRowStrings(t, eng,
		`MATCH (b:Person)<-[r:KNOWS]-(a:Person) `+reciprocalProjection,
		"an", "bn", "rid", "tag", "sn", "en")
	assertRows(t, "reverse", got, []string{
		"A B 1 AB A B",
		"B A 2 BA B A",
		"B C 3 BC B C",
	})
}

// TestReciprocalRelBinding_Undirected is the second RED case: two rows with
// DISTINCT ids reported the SAME properties, so edge 2 (tag BA) came back
// tagged AB.
func TestReciprocalRelBinding_Undirected(t *testing.T) {
	eng := newReciprocalEngine(t)
	got := collectRowStrings(t, eng,
		`MATCH (a:Person {name:'A'})-[r:KNOWS]-(b:Person) `+reciprocalProjection,
		"an", "bn", "rid", "tag", "sn", "en")
	assertRows(t, "undirected", got, []string{
		"A B 1 AB A B",
		"A B 2 BA B A",
	})
}

// TestReciprocalRelBinding_Simple pins the same three directions on a SIMPLE
// (non-multigraph) adjacency. A reciprocal pair is legal there too — the two
// edges occupy different pairs — so the defect reproduced identically, and the
// fix must hold without multigraph semantics.
func TestReciprocalRelBinding_Simple(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSeed(t, eng, reciprocalSeed)
	for _, tc := range []struct {
		name, pattern string
		want          []string
	}{
		{"forward", `MATCH (a:Person)-[r:KNOWS]->(b:Person) `, []string{"A B 1 AB A B", "B A 2 BA B A", "B C 3 BC B C"}},
		{"reverse", `MATCH (b:Person)<-[r:KNOWS]-(a:Person) `, []string{"A B 1 AB A B", "B A 2 BA B A", "B C 3 BC B C"}},
		{"undirected", `MATCH (a:Person {name:'A'})-[r:KNOWS]-(b:Person) `, []string{"A B 1 AB A B", "A B 2 BA B A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.pattern+reciprocalProjection,
				"an", "bn", "rid", "tag", "sn", "en")
			assertRows(t, tc.name, got, tc.want)
		})
	}
}

// TestReciprocalRelBinding_DurableStore repeats the three directions over the
// WAL-backed engine. #2500-#2503 showed the durable adapter can diverge from
// the in-memory one on exactly this class of by-handle routing, so the durable
// write path is pinned separately rather than assumed equivalent.
func TestReciprocalRelBinding_DurableStore(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	for _, q := range reciprocalSeed {
		res, rerr := eng.RunInTx(context.Background(), q, nil)
		if rerr != nil {
			t.Fatalf("seed %q: %v", q, rerr)
		}
		for res.Next() { // drain before commit
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("seed commit %q: %v", q, cerr)
		}
	}
	for _, tc := range []struct {
		name, pattern string
		want          []string
	}{
		{"forward", `MATCH (a:Person)-[r:KNOWS]->(b:Person) `, []string{"A B 1 AB A B", "B A 2 BA B A", "B C 3 BC B C"}},
		{"reverse", `MATCH (b:Person)<-[r:KNOWS]-(a:Person) `, []string{"A B 1 AB A B", "B A 2 BA B A", "B C 3 BC B C"}},
		{"undirected", `MATCH (a:Person {name:'A'})-[r:KNOWS]-(b:Person) `, []string{"A B 1 AB A B", "A B 2 BA B A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.pattern+reciprocalProjection,
				"an", "bn", "rid", "tag", "sn", "en")
			assertRows(t, tc.name, got, tc.want)
		})
	}
}

// relProjection reads the four attributes the defect corrupted off whichever
// relationship variable the query bound, ordered by the one attribute that was
// never wrong (the id).
const relProjection = `RETURN id(r) AS rid, r.tag AS tag, startNode(r).name AS sn, ` +
	`endNode(r).name AS en ORDER BY id(r)`

// TestReciprocalRelBinding_RelatedPaths covers the other operators that bind a
// relationship against storage direction, each over the same reciprocal pair:
// the expand-into hop, the variable-length legs, the named-path materialiser,
// and both shortest-path forms. Every one of them routes its attribute reads
// through the hydrator this fix corrected, so each is pinned rather than
// assumed to follow.
func TestReciprocalRelBinding_RelatedPaths(t *testing.T) {
	eng := newReciprocalEngine(t)
	const ab, ba, bc = "1 AB A B", "2 BA B A", "3 BC B C"
	for _, tc := range []struct {
		name, q string
		want    []string
	}{
		{
			"expand-into-reverse",
			`MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) MATCH (b)<-[r:KNOWS]-(a) ` + relProjection,
			[]string{ab},
		},
		{
			"expand-into-undirected",
			`MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) MATCH (a)-[r:KNOWS]-(b) ` + relProjection,
			[]string{ab, ba},
		},
		{
			"varlen-reverse",
			`MATCH (b:Person)<-[rs:KNOWS*1..1]-(a:Person) UNWIND rs AS r ` + relProjection,
			[]string{ab, ba, bc},
		},
		{
			"varlen-undirected",
			`MATCH (a:Person {name:'A'})-[rs:KNOWS*1..1]-(b:Person) UNWIND rs AS r ` + relProjection,
			[]string{ab, ba},
		},
		{
			"named-path-reverse",
			`MATCH p=(b:Person)<-[q:KNOWS]-(a:Person) UNWIND relationships(p) AS r ` + relProjection,
			[]string{ab, ba, bc},
		},
		{
			"shortest-path-undirected",
			`MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) ` +
				`MATCH p=shortestPath((a)-[:KNOWS*1..3]-(b)) UNWIND relationships(p) AS r ` + relProjection,
			[]string{ab},
		},
		{
			"all-shortest-paths-undirected",
			`MATCH (a:Person {name:'A'}), (b:Person {name:'B'}) ` +
				`MATCH p=allShortestPaths((a)-[:KNOWS*1..3]-(b)) UNWIND relationships(p) AS r ` + relProjection,
			[]string{ab, ba},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.q, "rid", "tag", "sn", "en")
			assertRows(t, tc.name, got, tc.want)
		})
	}
}

// TestReciprocalRelBinding_GoAPIEdges pins the orientation ladder's third rung.
// A graph built through the Go API stamps a handle on the slot but records the
// type in the per-pair store only, so no by-handle type record exists on either
// orientation and the handle cannot be matched against one. The adjacency's own
// handle column is then the only thing that can tell the two edges of a
// reciprocal pair apart, and it does.
func TestReciprocalRelBinding_GoAPIEdges(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"A", "B"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", k, err)
		}
	}
	if _, err := g.AddEdgeH("A", "B", 1); err != nil {
		t.Fatalf("AddEdgeH(A,B): %v", err)
	}
	if _, err := g.AddEdgeH("B", "A", 1); err != nil {
		t.Fatalf("AddEdgeH(B,A): %v", err)
	}
	g.SetEdgeLabel("A", "B", "KNOWS")
	g.SetEdgeLabel("B", "A", "KNOWS")
	if err := g.SetEdgeProperty("A", "B", "tag", lpg.StringValue("AB")); err != nil {
		t.Fatalf("SetEdgeProperty(A,B): %v", err)
	}
	if err := g.SetEdgeProperty("B", "A", "tag", lpg.StringValue("BA")); err != nil {
		t.Fatalf("SetEdgeProperty(B,A): %v", err)
	}
	eng := cypher.NewEngine(g)
	got := collectRowStrings(t, eng,
		`MATCH (b:Person)<-[r:KNOWS]-(a:Person) `+
			`RETURN a.name AS an, b.name AS bn, id(r) AS rid, r.tag AS tag, `+
			`startNode(r).name AS ssn, endNode(r).name AS sen ORDER BY id(r)`,
		"an", "bn", "rid", "tag", "ssn", "sen")
	assertRows(t, "go-api reverse", got, []string{"A B 1 AB A B", "B A 2 BA B A"})
}

// TestReciprocalRelBinding_WithBarrier pins a SECOND materialiser. Projecting
// the relationship through a `WITH` barrier binds it as a whole entity, which
// is hydrated by the projection's own boxed-relationship path rather than by
// the row hydrator the patterns above exercise — and that path carried its own,
// older orientation probe ("the forward pair has no labels and no properties,
// so the edge must be stored the other way"), which a reciprocal pair passes
// trivially because the traversal pair really does hold an edge of its own.
//
// The barrier matters beyond the projection shape: after it only `r` is in
// scope where startNode(r) and endNode(r) are evaluated, so the endpoints come
// from the RELATIONSHIP and an implementation that echoed the pattern's own
// variables could not satisfy them.
func TestReciprocalRelBinding_WithBarrier(t *testing.T) {
	eng := newReciprocalEngine(t)
	const barrier = ` WITH a.name AS an, b.name AS bn, r AS r ` +
		`RETURN id(r) AS rid, r.tag AS tag, startNode(r).name AS sn, endNode(r).name AS en ORDER BY id(r)`
	for _, tc := range []struct {
		name, q string
		want    []string
	}{
		{"forward", `MATCH (a:Person)-[r:KNOWS]->(b:Person)` + barrier, []string{"1 AB A B", "2 BA B A", "3 BC B C"}},
		{"reverse", `MATCH (b:Person)<-[r:KNOWS]-(a:Person)` + barrier, []string{"1 AB A B", "2 BA B A", "3 BC B C"}},
		{"undirected", `MATCH (a:Person {name:'A'})-[r:KNOWS]-(b:Person)` + barrier, []string{"1 AB A B", "2 BA B A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collectRowStrings(t, eng, tc.q, "rid", "tag", "sn", "en")
			assertRows(t, tc.name, got, tc.want)
		})
	}
}
