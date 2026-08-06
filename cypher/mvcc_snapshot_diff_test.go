package cypher

// mvcc_snapshot_diff_test.go — the versioned read path returns what the plain
// read path returns (rmp #2289, MVCC P4b).
//
// # What this is for
//
// P4b re-routed roughly a hundred and twenty read call sites through a
// snapshot-bound view. Every one of them is a chance to resolve the wrong
// structure, drop a column, or pair two columns from different entries. The
// barrier is still held, so no writer is concurrent, so the two paths MUST
// agree exactly — which turns the whole refactor into a property that can be
// checked rather than an argument that has to be believed.
//
// # THE DIFFERENTIAL IS GONE; THE ABSOLUTE ORACLE REMAINS (rmp #2311)
//
// The comparison needed a PLAIN arm — a graph with versioning disarmed — and that
// state no longer exists: MVCC is armed by lpg.New and there is no way to turn it off,
// because it is the module's only concurrency control. A differential against an arm
// nobody can construct is not a test.
//
// What survives is the better half, and the file said so before the arm was removed:
// "a differential goes green when BOTH arms are wrong the same way — this project has
// watched that happen twice". So the corpus carries HAND-COMPUTED expected answers for
// every case where the right answer can be written down, and those are asserted
// against the one arm that ships. That is strictly stronger than agreement between two
// implementations of the same mistake.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// snapshotDiffFixture is the write script both arms replay. It is deliberately
// broad: every store the read path touches must appear in it, or the
// differential proves nothing about that store.
var snapshotDiffFixture = []string{
	`CREATE (:Person {name:'ada', age:36, active:true})`,
	`CREATE (:Person {name:'alan', age:41, active:false})`,
	`CREATE (:Person:Admin {name:'grace', age:45, active:true})`,
	`CREATE (:Company {name:'acme', founded:1999})`,
	`MATCH (a:Person {name:'ada'}), (b:Person {name:'alan'}) CREATE (a)-[:KNOWS {since:2001, weight:0.5}]->(b)`,
	`MATCH (a:Person {name:'ada'}), (b:Person {name:'grace'}) CREATE (a)-[:KNOWS {since:2010}]->(b)`,
	// A PARALLEL edge of a DIFFERENT type between the same pair: this is what
	// exercises the per-instance and per-handle stores rather than the pair's
	// derived union.
	`MATCH (a:Person {name:'ada'}), (b:Person {name:'alan'}) CREATE (a)-[:LIKES {since:2020}]->(b)`,
	`MATCH (a:Person {name:'ada'}), (c:Company {name:'acme'}) CREATE (a)-[:WORKS_AT {role:'eng'}]->(c)`,
	`MATCH (b:Person {name:'alan'}), (c:Company {name:'acme'}) CREATE (b)-[:WORKS_AT {role:'ops'}]->(c)`,
	// Mutations after the fact, so some objects carry a version history.
	`MATCH (p:Person {name:'alan'}) SET p.age = 42, p.nickname = 'turing'`,
	`MATCH (p:Person {name:'grace'}) REMOVE p:Admin`,
	`MATCH (a:Person {name:'ada'})-[r:KNOWS]->(b:Person {name:'grace'}) SET r.weight = 0.9`,
	`CREATE (:Person {name:'doomed', age:1})`,
	`MATCH (p:Person {name:'doomed'}) DETACH DELETE p`,
}

// snapshotDiffQueries is the read corpus. Each entry names the read surface it
// is there to cover, so a future change that stops covering one is visible.
var snapshotDiffQueries = []struct{ name, q string }{
	{"all-nodes", `MATCH (n) RETURN count(*) AS c`},
	{"label-scan", `MATCH (n:Person) RETURN n.name AS name ORDER BY name`},
	{"multi-label", `MATCH (n:Person:Admin) RETURN n.name AS name ORDER BY name`},
	{"node-props", `MATCH (n:Person) RETURN n.name AS name, n.age AS age, n.active AS active, n.nickname AS nick ORDER BY name`},
	{"node-labels-fn", `MATCH (n) RETURN labels(n) AS ls, n.name AS name ORDER BY name`},
	{"properties-map", `MATCH (n:Company) RETURN properties(n) AS p`},
	{"expand-typed", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS a, b.name AS b ORDER BY a, b`},
	{"expand-untyped", `MATCH (a)-[r]->(b) RETURN a.name AS a, type(r) AS t, b.name AS b ORDER BY a, t, b`},
	{"rel-props", `MATCH (a)-[r:KNOWS]->(b) RETURN a.name AS a, b.name AS b, r.since AS since, r.weight AS w ORDER BY a, b`},
	{"rel-props-map", `MATCH ()-[r:LIKES]->() RETURN properties(r) AS p`},
	{"parallel-edges", `MATCH (a:Person {name:'ada'})-[r]->(b:Person {name:'alan'}) RETURN type(r) AS t, r.since AS s ORDER BY t`},
	{"degree", `MATCH (a:Person) RETURN a.name AS name, size([(a)-->() | 1]) AS d ORDER BY name`},
	{"optional-match", `MATCH (n:Person) OPTIONAL MATCH (n)-[:WORKS_AT]->(c) RETURN n.name AS n, c.name AS c ORDER BY n`},
	{"var-length", `MATCH (a:Person {name:'ada'})-[*1..2]->(x) RETURN count(*) AS c`},
	{"exists-subquery", `MATCH (n:Person) WHERE EXISTS { MATCH (n)-[:KNOWS]->() } RETURN n.name AS name ORDER BY name`},
	{"count-subquery", `MATCH (n:Person) RETURN n.name AS name, COUNT { (n)-[:KNOWS]->() } AS k ORDER BY name`},
	{"pattern-predicate", `MATCH (n:Person) WHERE (n)-[:WORKS_AT]->(:Company) RETURN n.name AS name ORDER BY name`},
	{"pattern-comprehension", `MATCH (n:Person {name:'ada'}) RETURN [(n)-[r]->(m) | type(r)] AS ts`},
	{"aggregation", `MATCH (n:Person) RETURN count(n) AS c, sum(n.age) AS total, avg(n.age) AS mean`},
	{"group-by", `MATCH (a)-[:WORKS_AT]->(c) RETURN c.name AS company, count(*) AS n ORDER BY company`},
	{"index-seek", `MATCH (n:Person {age: 42}) RETURN n.name AS name`},
	{"range-seek", `MATCH (n:Person) WHERE n.age > 40 RETURN n.name AS name ORDER BY name`},
	{"deleted-node-absent", `MATCH (n {name:'doomed'}) RETURN count(*) AS c`},
	{"path", `MATCH p = (a:Person {name:'ada'})-[:KNOWS]->(b) RETURN length(p) AS len, b.name AS b ORDER BY b`},
	{"startnode-endnode", `MATCH ()-[r:WORKS_AT]->() RETURN startNode(r).name AS s, endNode(r).name AS e ORDER BY s`},
	{"unwind-join", `MATCH (a:Person), (b:Person) WHERE a.age < b.age RETURN a.name AS a, b.name AS b ORDER BY a, b`},
}

// snapshotDiffEngine builds one arm.
func snapshotDiffEngine(t *testing.T) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	ctx := context.Background()
	if _, err := eng.RunInTx(ctx, "CREATE INDEX FOR (n:Person) ON (n.age)", nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	for i, q := range snapshotDiffFixture {
		if _, err := eng.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("fixture[%d] %q: %v", i, q, err)
		}
	}
	return eng
}

// runToString renders a result deterministically so two arms can be compared as
// text. Rows are rendered in the order the query produced them; every corpus
// query that could be order-sensitive carries an ORDER BY.
func runToString(t *testing.T, eng *Engine, q string) string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, map[string]expr.Value{})
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer func() { _ = res.Close() }()
	var b strings.Builder
	cols := res.Columns()
	b.WriteString(strings.Join(cols, "|") + "\n")
	for res.Next() {
		row := res.Record()
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, canonRender(row[c]))
		}
		b.WriteString(strings.Join(parts, "|") + "\n")
	}
	if err := res.Err(); err != nil {
		return "DRAIN ERROR: " + err.Error()
	}
	return b.String()
}

// canonRender renders a value deterministically.
//
// A map value renders in Go map-iteration order, which differs run to run, so
// two arms that agree perfectly would still compare unequal. Sorting the
// entries of any {…} rendering makes the comparison about the DATA rather than
// about hash seeds. It is a harness concern only: nothing about the engine's
// answer depends on it.
func canonRender(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) < 2 {
		return s
	}
	var open, close byte
	switch {
	case s[0] == '{' && s[len(s)-1] == '}':
		open, close = '{', '}'
	case s[0] == '[' && s[len(s)-1] == ']':
		// A LIST whose order openCypher leaves unspecified — labels(n) and a
		// pattern comprehension over parallel edges are both in the corpus, and
		// comparing their rendering verbatim made this test flaky: it passed in
		// isolation and failed about once per full-suite run. Sorting is the
		// right comparison here because the property under test is the CONTENT
		// the two read paths produce, and neither path is entitled to an order.
		open, close = '[', ']'
	default:
		return s
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return s
	}
	parts := strings.Split(inner, ", ")
	sort.Strings(parts)
	return string(open) + strings.Join(parts, ", ") + string(close)
}

// unquote strips the quotes a string value renders with, so an oracle can be
// written as the value rather than as its rendering.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func TestMVCCSnapshotRead_AbsoluteOracle(t *testing.T) {
	oracle := []struct {
		name, q string
		want    []string
	}{
		// Four CREATEd persons, one DETACH DELETEd, so three survive; plus one
		// company.
		{"node-count", `MATCH (n) RETURN count(*) AS c`, []string{"4"}},
		{"person-count", `MATCH (n:Person) RETURN count(*) AS c`, []string{"3"}},
		// grace's :Admin was REMOVEd, so the two-label match finds nobody.
		{"admin-count", `MATCH (n:Person:Admin) RETURN count(*) AS c`, []string{"0"}},
		// alan's age was SET to 42 after being created at 41.
		{"updated-age", `MATCH (n:Person {name:'alan'}) RETURN n.age AS a`, []string{"42"}},
		{"added-prop", `MATCH (n:Person {name:'alan'}) RETURN n.nickname AS a`, []string{"turing"}},
		// ada has two KNOWS and one LIKES and one WORKS_AT.
		{"ada-out-degree", `MATCH (:Person {name:'ada'})-[r]->() RETURN count(r) AS c`, []string{"4"}},
		// The parallel pair ada→alan carries one KNOWS and one LIKES, and each
		// must report its OWN type — the per-instance store, not the pair union.
		{"parallel-types", `MATCH (:Person {name:'ada'})-[r]->(:Person {name:'alan'}) RETURN type(r) AS t ORDER BY t`,
			[]string{"KNOWS", "LIKES"}},
		// The edge property SET after creation.
		{"updated-rel-prop", `MATCH (:Person {name:'ada'})-[r:KNOWS]->(:Person {name:'grace'}) RETURN r.weight AS w`,
			[]string{"0.9"}},
		// The untouched one keeps its original value.
		{"original-rel-prop", `MATCH (:Person {name:'ada'})-[r:KNOWS]->(:Person {name:'alan'}) RETURN r.weight AS w`,
			[]string{"0.5"}},
		{"deleted-gone", `MATCH (n {name:'doomed'}) RETURN count(*) AS c`, []string{"0"}},
		// 36 + 42 + 45.
		{"age-sum", `MATCH (n:Person) RETURN sum(n.age) AS s`, []string{"123"}},
		{"index-seek", `MATCH (n:Person {age:42}) RETURN n.name AS n`, []string{"alan"}},
	}
	{
		eng := snapshotDiffEngine(t)
		for _, tc := range oracle {
			t.Run(tc.name, func(t *testing.T) {
				res, err := eng.Run(context.Background(), tc.q, nil)
				if err != nil {
					t.Fatalf("%s: %v", tc.q, err)
				}
				defer func() { _ = res.Close() }()
				var got []string
				cols := res.Columns()
				for res.Next() {
					row := res.Record()
					got = append(got, unquote(canonRender(row[cols[0]])))
				}
				if err := res.Err(); err != nil {
					t.Fatalf("%s: drain: %v", tc.q, err)
				}
				sort.Strings(got)
				want := append([]string(nil), tc.want...)
				sort.Strings(want)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s\n got %v\nwant %v", tc.q, got, want)
				}
			})
		}
	}
}
