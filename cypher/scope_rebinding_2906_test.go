package cypher_test

// scope_rebinding_2906_test.go — rmp #2906 and rmp #2914.
//
// WITH ends the scope of every variable it does not project, and a later clause
// may then introduce the same name as a NEW variable (openCypher 9, WITH: "only
// the projected variables are available to the following clauses"). The two
// variables are unrelated, so a query is equivalent to the same query with the
// later variable renamed.
//
// The physical builder keeps per-variable facts keyed by NAME — "this name is a
// scalar column", "this name is a relationship, reconstruct it from these
// columns" — and they were query-wide. The later variable's facts reached plans
// built for the earlier one, resolved lazily after the whole build (rmp #2906:
// `MATCH (x:N {k: 'a'}) WITH 1 AS one MATCH (a:N) UNWIND [1] AS x RETURN a`
// filtered x as a scalar and returned nothing), and the earlier variable's facts
// were inherited by the later one (`MATCH ()-[x]->() WITH 1 AS one UNWIND [1] AS
// x RETURN x` reconstructed 1 as a relationship and returned null).
//
// TestScopeRebinding_* crosses every earlier binding form with every later
// binder form and asserts, for each pair, that the query with the name reused
// and the query with the later name renamed both return the answer openCypher
// requires. CALL … YIELD is a control: it was already correct.
//
// rmp #2914 is the projection half of the same class: an item `<expr> AS x`
// where x is bound in the input read x's slot instead of evaluating <expr>, for
// a literal, a parameter or any expression the guard did not list —
// `MATCH (x:N) RETURN 1 AS x` returned the node.
//
// Layer: short. goleak-clean (engines and graphs are local).

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// srEarlier is a clause sequence that binds x, and the number of rows it yields.
type srEarlier struct {
	clauses string
	rows    int
}

// srEarliers binds x as a node, a relationship, a path, a variable-length
// relationship list, a scalar and an aggregate, each with and without a filter
// on x — the filter is what #2906 needed to go wrong.
var srEarliers = []srEarlier{
	{`MATCH (x:N)`, 3},
	{`MATCH (x:N {k: 'a'})`, 1},
	{`MATCH (x:N) WHERE x.k = 'a'`, 1},
	{`MATCH ()-[x:R]->()`, 3},
	{`MATCH ()-[x:R {s: 'b'}]->()`, 1},
	{`MATCH ()-[x:R]->() WHERE x.s = 'b'`, 1},
	{`MATCH x = (:N)-[:R]->()`, 3},
	{`MATCH x = (:N {k: 'a'})-[:R]->() WHERE length(x) = 1`, 1},
	{`MATCH (:N {k: 'c'})-[x:R*1..2]->()`, 2},
	{`MATCH (:N {k: 'c'})-[x:R*1..2]->() WHERE x[1] IS NOT NULL`, 1},
	{`UNWIND [7] AS x`, 1},
	{`UNWIND [7, 8] AS x WITH x WHERE x = 7`, 1},
	{`MATCH (n:N) WITH count(n) AS x`, 1},
	{`MATCH (n:N) WITH n.k AS x, count(*) AS c`, 3},
	{`MATCH (n:N) WITH n.k AS x, count(*) AS c WHERE x = 'a'`, 1},
}

// srLater introduces the name {X} after the WITH; value is what it returns per
// earlier row. agg marks a later clause that aggregates the earlier rows into
// one, returning their count; once marks one that groups them into a single row
// returning value.
type srLater struct {
	clauses string
	value   string
	agg     bool
	once    bool
}

var srLaters = []srLater{
	{clauses: `UNWIND [5] AS {X} RETURN {X} AS v`, value: `5`},
	{clauses: `MATCH (a:N {k: 'c'}) UNWIND [5] AS {X} RETURN a.k AS v`, value: `"c"`},
	{clauses: `MATCH ({X}:N {k: 'b'}) RETURN {X}.k AS v`, value: `"b"`},
	{clauses: `MATCH ({X}:N) WHERE {X}.k = 'b' RETURN {X}.k AS v`, value: `"b"`},
	{clauses: `MATCH ({X}:N {k: 'b'}) RETURN labels({X}) AS v`, value: `["N"]`},
	{clauses: `MATCH (:N {k: 'a'})-[{X}:R]->() RETURN {X}.s AS v`, value: `"a"`},
	{clauses: `MATCH (:N {k: 'a'})-[{X}:R]->() RETURN type({X}) AS v`, value: `"R"`},
	{clauses: `MATCH {X} = (:N {k: 'b'})-[:R]->() RETURN length({X}) AS v`, value: `1`},
	{clauses: `MATCH (:N {k: 'b'})-[{X}:R*1..1]->() RETURN [r IN {X} | r.s] AS v`, value: `["b"]`},
	{clauses: `WITH one, 2 + one AS {X} RETURN {X} AS v`, value: `3`},
	{clauses: `MATCH (a:N {k: 'c'}) WITH a, 1 + one AS {X} RETURN a.k + toString({X}) AS v`, value: `"c2"`},
	{clauses: `WITH count(*) AS {X} RETURN {X} AS v`, agg: true},
	{clauses: `MATCH (a:N {k: 'c'}) WITH a, count(*) AS {X} RETURN a.k AS v`, value: `"c"`, once: true},
	{clauses: `RETURN [{X} IN [4] | {X} + one] AS v`, value: `[5]`},
	{clauses: `MATCH (a:N {k: 'c'}) RETURN [{X} IN [4] | a.k] AS v`, value: `["c"]`},
	// Control: CALL … YIELD was already correct.
	{clauses: `CALL db.labels() YIELD label AS {X} WITH {X} WHERE {X} = 'N' RETURN {X} AS v`, value: `"N"`},
}

// srExpect is what a later clause returns after earlier rows.
func srExpect(l srLater, rows int) []string {
	if l.agg {
		return []string{strconv.Itoa(rows)}
	}
	if l.once {
		return []string{l.value}
	}
	out := make([]string, rows)
	for i := range out {
		out[i] = l.value
	}
	return out
}

func TestScopeRebinding_LaterBinderAfterWith(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, e := range srEarliers {
		for _, l := range srLaters {
			want := strings.Join(srExpect(l, e.rows), ";")
			for _, name := range []string{"x", "z"} {
				q := e.clauses + ` WITH 1 AS one ` + strings.ReplaceAll(l.clauses, "{X}", name)
				if got := strings.Join(ipRows(t, eng, q, "v"), ";"); got != want {
					t.Errorf("%q\n  got  [%s]\n  want [%s]", q, got, want)
				}
			}
		}
	}
}

// TestScopeRebinding_Foreach covers FOREACH, whose loop variable is introduced in
// a body private to the clause. Each query runs on a fresh engine so the writes
// of one cannot reach another.
func TestScopeRebinding_Foreach(t *testing.T) {
	t.Parallel()
	for _, e := range srEarliers {
		for _, name := range []string{"x", "z"} {
			eng := ipEngine(t, false)
			q := e.clauses + ` WITH 1 AS one FOREACH (` + name + ` IN [3] | CREATE (:Z {v: ` + name + ` + one})) RETURN one AS v`
			res, err := eng.RunInTx(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("RunInTx(%q): %v", q, err)
			}
			n := 0
			for res.Next() {
				n++
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Err(%q): %v", q, err)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("Close(%q): %v", q, err)
			}
			if n != e.rows {
				t.Errorf("%q: %d rows, want %d", q, n, e.rows)
			}
			want := strings.Join(srExpect(srLater{value: `4`}, e.rows), ";")
			if got := strings.Join(ipRows(t, eng, `MATCH (z:Z) RETURN z.v AS v`, "v"), ";"); got != want {
				t.Errorf("%q: created [%s], want [%s]", q, got, want)
			}
		}
	}
}

// TestScopeRebinding_EarlierScopeUnaffected pins the reported direction on its
// own: the earlier variable's filter below the WITH must keep reading it as the
// entity it is, whatever a later clause binds to the same name.
func TestScopeRebinding_EarlierScopeUnaffected(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, tc := range []struct{ query, want string }{
		{`MATCH (x:N {k: 'a'}) WITH 1 AS one MATCH (a:N) UNWIND [1] AS x RETURN a.k AS v`, `"a";"b";"c"`},
		{`MATCH (x:N) WHERE x.k = 'a' WITH 1 AS one MATCH (a:N) WITH a, 1 + 1 AS x RETURN a.k AS v`, `"a";"b";"c"`},
		{`MATCH (x:N) WHERE x.k = 'a' WITH 1 AS one MATCH (a:N) WITH a, count(*) AS x RETURN a.k AS v`, `"a";"b";"c"`},
		{`MATCH (x:N {k: 'a'}) WITH 1 AS one RETURN [x IN [1, 2] | x] AS v`, `[1, 2]`},
		{`MATCH ()-[x:R {s: 'b'}]->() WITH 1 AS one UNWIND [1] AS x RETURN x AS v`, `1`},
		{`MATCH x = (:N {k: 'a'})-->() WITH 1 AS one UNWIND [1] AS x RETURN x AS v`, `1`},
		{`UNWIND [1] AS x WITH 1 AS one MATCH (x:N) WHERE x.k = 'a' RETURN x.k AS v`, `"a"`},
		{`MATCH (x:N {k: 'a'}) WITH 1 AS one MATCH (x:N) RETURN x.k AS v`, `"a";"b";"c"`},
	} {
		if got := strings.Join(ipRows(t, eng, tc.query, "v"), ";"); got != tc.want {
			t.Errorf("%q: got [%s], want [%s]", tc.query, got, tc.want)
		}
	}
}

// TestScopeRebinding_UnionBranches asserts that a UNION branch does not inherit
// the facts of the other branch's variable of the same name.
func TestScopeRebinding_UnionBranches(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, tc := range []struct{ query, want string }{
		{`UNWIND [7] AS x RETURN 'u' AS v UNION ALL MATCH (x:N) WHERE x.k = 'a' RETURN x.k AS v`, `"a";"u"`},
		{`MATCH ()-[x:R {s: 'b'}]->() RETURN 'r' AS v UNION ALL UNWIND [7] AS x RETURN toString(x) AS v`, `"7";"r"`},
		{`MATCH (x:N {k: 'a'}) RETURN x.k AS v UNION UNWIND ['q'] AS x RETURN x AS v`, `"a";"q"`},
	} {
		if got := strings.Join(ipRows(t, eng, tc.query, "v"), ";"); got != tc.want {
			t.Errorf("%q: got [%s], want [%s]", tc.query, got, tc.want)
		}
	}
}

// TestProjectionAlias_ShadowsBoundName is rmp #2914: `<expr> AS x` over an input
// that binds x evaluates <expr>, in RETURN and in WITH, whatever shape <expr> is.
func TestProjectionAlias_ShadowsBoundName(t *testing.T) {
	t.Parallel()
	eng := ipEngine(t, false)
	for _, tc := range []struct{ expr, want string }{
		{`1`, `1`},
		{`'q'`, `"q"`},
		{`$pk`, `"a"`},
		{`true`, `true`},
		{`null`, `null`},
		{`1 + 1`, `2`},
		{`[1, 2]`, `[1, 2]`},
		{`{a: 1}.a`, `1`},
		{`toUpper('q')`, `"Q"`},
		{`CASE WHEN true THEN 9 END`, `9`},
		{`size([1, 2, 3])`, `3`},
	} {
		for _, q := range []string{
			`MATCH (x:N) RETURN ` + tc.expr + ` AS x`,
			`MATCH (x:N) WITH ` + tc.expr + ` AS x RETURN x`,
			`MATCH (x:N) WITH x, ` + tc.expr + ` AS y WITH y AS x RETURN x`,
			`MATCH ()-[x:R]->() RETURN ` + tc.expr + ` AS x`,
			`MATCH x = (:N)-[:R]->() RETURN ` + tc.expr + ` AS x`,
			`UNWIND [7, 8, 9] AS x RETURN ` + tc.expr + ` AS x`,
		} {
			want := strings.Join([]string{tc.want, tc.want, tc.want}, ";")
			if got := strings.Join(ipRows(t, eng, q, "x"), ";"); got != want {
				t.Errorf("%q: got [%s], want [%s]", q, got, want)
			}
		}
	}
	for _, tc := range []struct{ query, want string }{
		// Another bound variable aliased to x.
		{`MATCH (x:N {k: 'a'}), (y:N {k: 'b'}) RETURN y.k AS x`, `"b"`},
		{`MATCH (x:N {k: 'a'}), (y:N {k: 'b'}) WITH y AS x RETURN x.k AS x`, `"b"`},
		// A scalar re-projected under its own name.
		{`UNWIND [1] AS x WITH x + 1 AS x RETURN x`, `2`},
		{`WITH 1 AS x WITH 2 AS x RETURN x`, `2`},
		// The re-projection of the same item keeps reading its slot.
		{`MATCH (n:N) WITH n, n.k AS x ORDER BY x WITH n.k AS x RETURN x`, `"a";"b";"c"`},
		{`MATCH (n:N) WITH n.k AS x, count(*) AS c RETURN x, c AS n`, `"a";"b";"c"`},
	} {
		if got := strings.Join(ipRows(t, eng, tc.query, "x"), ";"); got != tc.want {
			t.Errorf("%q: got [%s], want [%s]", tc.query, got, tc.want)
		}
	}
}
