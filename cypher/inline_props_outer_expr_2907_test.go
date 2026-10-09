package cypher_test

// inline_props_outer_expr_2907_test.go — rmp #2907.
//
// An inline property map `(x {k: v})` is the predicate `x.k = v` (openCypher 9,
// §3.2 "Node patterns": the map constrains the properties of the matched
// entity), so it must answer exactly what `WHERE x.k = v` answers, whatever
// expression v is. When a pattern shares no variable with the preceding clauses
// it is joined by a plain Apply whose inner arm is built against a fresh schema,
// and a property equality that reads an outer variable is hoisted above that
// Apply. The hoisting test recognised only a bare variable and a property chain,
// so `(x:N {k: p[0]})` after `UNWIND [['a','b']] AS p` kept its equality in the
// inner arm, where p reads as null, and matched nothing.
//
// A second defect sat behind the first. Once the plain Apply's inner arm is
// built, the relationship metadata it registered is rebased by the outer width
// for the operators above the join — and a row-binding plan made INSIDE the arm
// resolved that metadata lazily at its first row, after the rebase, so it read
// the relationship from the wrong slot of its inner-only row. Any relationship
// property map that stays in the arm — a constant or a parameter — therefore
// matched nothing after a preceding clause:
// `WITH 1 AS one MATCH (a)-[r {s: 'a'}]->(b)`.
//
// Every value expression below is checked against the WHERE form of the same
// predicate, on nodes and on relationships, with and without an index on the
// node property. Each WHERE baseline is also pinned to a non-empty answer, so a
// route and its baseline cannot agree on an empty result.
//
// Layer: short. goleak-clean (engine and graph are local).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ipDrivers bind the outer variables every value expression reads: p a list, m
// a map, u a string, i0 an integer. Each driver binds them differently, so the
// hoisting is exercised after a leading WITH, after an UNWIND and after a MATCH.
var ipDrivers = []string{
	`WITH ['a', 'b'] AS p, {q: 'a', n: 1} AS m, 'A' AS u, 0 AS i0`,
	`UNWIND [['a', 'b']] AS p WITH p, {q: 'a', n: 1} AS m, 'A' AS u, 0 AS i0`,
	`MATCH (o:O) WITH o.p AS p, {q: 'a', n: o.n} AS m, o.u AS u, o.i0 AS i0`,
}

// ipValues are value expressions for the node property k (a string), each
// evaluating to 'a' under every driver. The constant and the parameter read no
// outer variable: they stay in the inner arm of the Apply, and they cover the
// second half of the defect (see [TestInlinePropertyMap_InnerArmRelationship]).
var ipValues = []string{
	`'a'`,
	`p[0]`,
	`p[i0]`,
	`p[-2]`,
	`m.q`,
	`m['q']`,
	`toLower(u)`,
	`head(p)`,
	`coalesce(null, p[0])`,
	`p[0] + ''`,
	`CASE WHEN i0 = 0 THEN p[0] ELSE 'z' END`,
	`[z IN p | z][0]`,
	`p[0..1][0]`,
	`reduce(s = '', z IN p[0..1] | s + z)`,
	`$pk`,
}

// ipIntValues are value expressions for the integer property i, each
// evaluating to 1.
var ipIntValues = []string{
	`1`,
	`i0 + 1`,
	`m.n`,
	`size(p) - 1`,
	`-(i0 - 1)`,
	`abs(i0 - 1)`,
}

// ipListValues are value expressions for the list property l = ['a', 'b'].
var ipListValues = []string{
	`['a', 'b']`,
	`p`,
	`[p[0], p[1]]`,
	`[p[0], 'b']`,
	`p[0..2]`,
}

var ipParams = map[string]expr.Value{"pk": expr.StringValue("a")}

func ipEngine(t *testing.T, indexed bool) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:O {p: ['a', 'b'], u: 'A', i0: 0, n: 1})`,
		`CREATE (a:N {k: 'a', i: 1, l: ['a', 'b']}), (b:N {k: 'b', i: 2, l: ['b']}), (c:N {k: 'c', i: 3, l: []}),
		        (a)-[:R {s: 'a', i: 1, l: ['a', 'b']}]->(b), (b)-[:R {s: 'b', i: 2, l: ['b']}]->(c),
		        (c)-[:R {s: 'a', i: 1, l: ['a', 'b']}]->(a)`,
	} {
		ipExec(t, eng, q)
	}
	if indexed {
		for _, q := range []string{
			`CREATE INDEX FOR (n:N) ON (n.k)`,
			`CREATE INDEX FOR (n:N) ON (n.i)`,
		} {
			res, err := eng.RunAny(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("%s: close: %v", q, err)
			}
		}
	}
	return eng
}

func ipExec(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
}

// ipRows runs query and returns its column c, rendered and sorted.
func ipRows(t *testing.T, eng *cypher.Engine, query, c string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, ipParams)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	var out []string
	for res.Next() {
		out = append(out, fmt.Sprintf("%v", res.Record()[c]))
	}
	rerr := res.Err()
	if cerr := res.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		t.Fatalf("Run(%q): %v", query, rerr)
	}
	sort.Strings(out)
	return out
}

// ipCase is one inline-map route and its WHERE-form baseline, with the answer
// the baseline is pinned to.
type ipCase struct {
	inline, where string
	want          []string
}

func ipCases(driver string) []ipCase {
	var cs []ipCase
	node := func(prop, v string) {
		cs = append(cs,
			ipCase{
				driver + ` MATCH (x:N {` + prop + `: ` + v + `}) RETURN x.k AS v`,
				driver + ` MATCH (x:N) WHERE x.` + prop + ` = ` + v + ` RETURN x.k AS v`,
				[]string{`"a"`},
			},
			// The map on the far end of a hop: the equality sits above an Expand.
			ipCase{
				driver + ` MATCH (:N {k: 'c'})-[:R]->(x:N {` + prop + `: ` + v + `}) RETURN x.k AS v`,
				driver + ` MATCH (:N {k: 'c'})-[:R]->(x:N) WHERE x.` + prop + ` = ` + v + ` RETURN x.k AS v`,
				[]string{`"a"`},
			},
			// Two keys, the second a constant.
			ipCase{
				driver + ` MATCH (x:N {` + prop + `: ` + v + `, k: 'a'}) RETURN x.k AS v`,
				driver + ` MATCH (x:N) WHERE x.` + prop + ` = ` + v + ` AND x.k = 'a' RETURN x.k AS v`,
				[]string{`"a"`},
			},
			ipCase{
				driver + ` OPTIONAL MATCH (x:N {` + prop + `: ` + v + `}) RETURN x.k AS v`,
				driver + ` OPTIONAL MATCH (x:N) WHERE x.` + prop + ` = ` + v + ` RETURN x.k AS v`,
				[]string{`"a"`},
			},
		)
	}
	rel := func(prop, v string) {
		cs = append(cs,
			ipCase{
				driver + ` MATCH (a:N)-[r:R {` + prop + `: ` + v + `}]->(b:N) RETURN a.k + b.k AS v`,
				driver + ` MATCH (a:N)-[r:R]->(b:N) WHERE r.` + prop + ` = ` + v + ` RETURN a.k + b.k AS v`,
				[]string{`"ab"`, `"ca"`},
			},
			ipCase{
				driver + ` MATCH (a:N)<-[r {` + prop + `: ` + v + `}]-(b:N) RETURN a.k + b.k AS v`,
				driver + ` MATCH (a:N)<-[r]-(b:N) WHERE r.` + prop + ` = ` + v + ` RETURN a.k + b.k AS v`,
				[]string{`"ac"`, `"ba"`},
			},
			ipCase{
				driver + ` MATCH (a:N {k: 'c'})-[:R*1..2 {` + prop + `: ` + v + `}]->(b:N) RETURN b.k AS v`,
				driver + ` MATCH (a:N {k: 'c'})-[rs:R*1..2]->(b:N) WHERE all(r IN rs WHERE r.` + prop + ` = ` + v + `) RETURN b.k AS v`,
				[]string{`"a"`, `"b"`},
			},
		)
	}
	for _, v := range ipValues {
		node("k", v)
		rel("s", v)
	}
	for _, v := range ipIntValues {
		node("i", v)
		rel("i", v)
	}
	for _, v := range ipListValues {
		node("l", v)
		rel("l", v)
	}
	return cs
}

func TestInlinePropertyMap_OuterExpressionValue(t *testing.T) {
	t.Parallel()
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%v", indexed), func(t *testing.T) {
			t.Parallel()
			eng := ipEngine(t, indexed)
			for di, driver := range ipDrivers {
				for _, c := range ipCases(driver) {
					want := ipRows(t, eng, c.where, "v")
					if strings.Join(want, ";") != strings.Join(c.want, ";") {
						t.Errorf("driver %d baseline %q\n  got  %v\n  want %v", di, c.where, want, c.want)
						continue
					}
					if got := ipRows(t, eng, c.inline, "v"); strings.Join(got, ";") != strings.Join(want, ";") {
						t.Errorf("driver %d %q\n  got  %v\n  want %v (WHERE form)", di, c.inline, got, want)
					}
				}
			}
		})
	}
}

// TestInlinePropertyMap_OuterExpressionValue_Reported pins the query rmp #2907
// was filed with.
func TestInlinePropertyMap_OuterExpressionValue_Reported(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, q := range []string{
		`UNWIND [['a','b']] AS p MATCH (x:N {k: p[0]}) RETURN x.k AS v`,
		`UNWIND [['a','b']] AS p MATCH (x:N) WHERE x.k = p[0] RETURN x.k AS v`,
		`UNWIND ['a'] AS k MATCH (x:N {k: k}) RETURN x.k AS v`,
	} {
		if got := ipRows(t, eng, q, "v"); strings.Join(got, ";") != `"a"` {
			t.Errorf("%q: got %v, want [a]", q, got)
		}
	}
}

// TestInlinePropertyMap_InnerArmRelationship pins the second defect on its own:
// a relationship bound inside the inner arm of a plain Apply, read by a
// predicate that stays in the arm, after each kind of preceding clause.
func TestInlinePropertyMap_InnerArmRelationship(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, driver := range []string{``, `WITH 1 AS one`, `UNWIND [1, 1] AS one WITH DISTINCT one`, `MATCH (o:O) WITH o`} {
		for _, tc := range []struct{ query, col, want string }{
			{`MATCH (a:N)-[r:R {s: 'a'}]->(b:N) RETURN a.k + b.k AS v`, "v", `"ab";"ca"`},
			{`MATCH (a:N)-[r:R {s: $pk}]->(b:N) RETURN r.i AS v`, "v", `1;1`},
			{`MATCH (a:N)<-[:R {i: 2}]-(b:N) RETURN a.k AS v`, "v", `"c"`},
			{`MATCH (a:N)-[:R {s: 'b'}]-(b:N) RETURN a.k + b.k AS v`, "v", `"bc";"cb"`},
			{`MATCH (a:N)-[:R {s: 'a'}]->(b:N)-[:R {s: 'b'}]->(c:N) RETURN a.k + b.k + c.k AS v`, "v", `"abc"`},
			{`MATCH (a:N {k: 'c'})-[:R*1..2 {s: 'a'}]->(b:N) RETURN b.k AS v`, "v", `"a";"b"`},
			{`MATCH (a:N {k: 'c'})-[rs:R*1..2 {s: $pk}]->(b:N) RETURN b.k AS v`, "v", `"a";"b"`},
		} {
			q := strings.TrimSpace(driver + " " + tc.query)
			if got := strings.Join(ipRows(t, eng, q, tc.col), ";"); got != tc.want {
				t.Errorf("%q: got [%s], want [%s]", q, got, tc.want)
			}
		}
	}
}
