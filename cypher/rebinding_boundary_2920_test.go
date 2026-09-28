package cypher_test

// rebinding_boundary_2920_test.go — rmp #2920, #2878, #2921, #2922 and #2877.
//
// One class of wrong answer: a name that crosses a projection boundary resolved
// to the binding it had BEFORE the boundary.
//
//   - #2920 and #2878. The physical builder keeps, per relationship or path
//     variable, the column coordinates it is reconstructed from. A Projection or
//     an EagerAggregation re-lays the row, and the coordinates survived it for
//     every name the boundary kept. When the boundary re-bound the name, the old
//     fact decoded the new layout: `MATCH (a:A)-[r]->(b) WITH b, 5 AS r RETURN
//     r` returned null, `MATCH p = (a:A)-[*]->(b) WITH b, [1, 2, 3, 4] AS p
//     RETURN p` returned an invented path, and `WITH count(*) AS r` failed with
//     `count() takes exactly 1 argument(s), got 0`. When the boundary carried
//     the variable, a NULL carried out of an OPTIONAL MATCH was rebuilt from the
//     old coordinates into a fabricated relationship. WITH forwards a variable
//     unchanged (clauses/with/With1.feature [3] "Forwarding a relationship
//     variable", [4] "Forwarding a path variable"), and an alias is the value of
//     its expression.
//   - #2921. Relationship uniqueness binds the relationships of ONE MATCH
//     pattern, including a relationship variable that pattern reuses from an
//     earlier clause (clauses/match/Match4.feature [7]). A relationship variable
//     of an earlier clause that the pattern does not name is not part of it, so
//     it excludes nothing: two parallel relationships, `MATCH ()-[r]->() WITH r
//     MATCH (a)-[:R*1..1]->(b)`, is 2 × 2 rows, and returned 2.
//   - #2922. The WHERE of a non-aggregating WITH is applied below the
//     projection with every alias replaced by its expression; the replacement
//     skipped subscripts, slices and every other compound expression, so the
//     alias stayed unbound there and `WITH [1] AS q WHERE q[0] = 1` filtered
//     every row out.
//   - #2877 was fixed by rmp #2906 (commit 3cef9714), which inverted its pin in
//     TestRowBindPlan_NameCollisionShapes; the UNWIND shapes here pin the path
//     and variable-length forms beside it.
//
// Every re-binding query is also run with the later name renamed to one the
// query never binds before, and the two must agree (the renamed query is a
// different query only by spelling).
//
// Layer: short. goleak-clean (engines and graphs are local).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// bndEngine is one (:A {n: 1})-[:R {w: 1}]->(:B {n: 2}) relationship and an
// isolated (:C), the anchor of the OPTIONAL MATCH shapes that bind NULL.
func bndEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ipExec(t, eng, `CREATE (:A {n: 1})-[:R {w: 1}]->(:B {n: 2}), (:C)`)
	return eng
}

// bndRows runs q and returns its column v, rendered and sorted, or the error.
func bndRows(eng *cypher.Engine, q string) (string, error) {
	return bndColumn(eng, q, "v")
}

// bndColumn runs q and returns its column c, rendered and sorted, or the error.
func bndColumn(eng *cypher.Engine, q, c string) (string, error) {
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		return "", err
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
		return "", rerr
	}
	sort.Strings(out)
	return strings.Join(out, ";"), nil
}

// bndCheck asserts that q returns want, and that q with every {X} spelled z —
// a name nothing binds before — returns want too.
func bndCheck(t *testing.T, eng *cypher.Engine, q, name, want string) {
	t.Helper()
	for _, n := range []string{name, "z"} {
		qq := strings.ReplaceAll(q, "{X}", n)
		got, err := bndRows(eng, qq)
		if err != nil {
			t.Errorf("%s\n  error: %v", qq, err)
			continue
		}
		if got != want {
			t.Errorf("%s\n  got  [%s]\n  want [%s]", qq, got, want)
		}
	}
}

// bndEarlier binds the name x as a relationship, a fixed-length path, a
// variable-length path or a variable-length relationship list.
var bndEarlier = []struct{ kind, match string }{
	{"relationship", `MATCH (a:A)-[x]->(b)`},
	{"fixed_path", `MATCH x = (a:A)-->(b)`},
	{"varlen_path", `MATCH x = (a:A)-[*1..1]->(b)`},
	{"varlen_rel", `MATCH (a:A)-[x*1..1]->(b)`},
}

// TestRebindingBoundary_ProjectionRebindsTheName is the #2920 matrix: every
// earlier binding form, re-bound by a WITH item of every value shape, in both
// item orders, read by RETURN, by the WITH's own WHERE, and through a second
// WITH.
func TestRebindingBoundary_ProjectionRebindsTheName(t *testing.T) {
	t.Parallel()
	eng := bndEngine(t)
	values := []struct{ expr, want string }{
		{`5`, `5`},
		{`[1, 2, 3, 4]`, `[1, 2, 3, 4]`},
		{`[7]`, `[7]`},
		{`b.n`, `2`},
		{`'s'`, `"s"`},
		{`{w: 9}.w`, `9`},
		{`null`, `null`},
	}
	for _, e := range bndEarlier {
		for _, v := range values {
			for _, shape := range []string{
				`WITH b, ` + v.expr + ` AS {X} RETURN {X} AS v`,
				`WITH ` + v.expr + ` AS {X}, b RETURN {X} AS v`,
				`WITH b, ` + v.expr + ` AS {X}, 0 AS y RETURN {X} AS v`,
				`WITH b, ` + v.expr + ` AS {X} WITH {X} RETURN {X} AS v`,
				`WITH b, ` + v.expr + ` AS {X} WITH b, {X} RETURN {X} AS v`,
				`WITH b, ` + v.expr + ` AS {X} RETURN [{X}] AS w, {X} AS v`,
			} {
				t.Run(e.kind, func(t *testing.T) {
					bndCheck(t, eng, e.match+` `+shape, "x", v.want)
				})
			}
		}
		// The alias read through an operator and a WHERE on the same WITH.
		t.Run(e.kind+"_where", func(t *testing.T) {
			bndCheck(t, eng, e.match+` WITH b, 5 AS {X} WHERE {X} = 5 RETURN {X} + 1 AS v`, "x", `6`)
			bndCheck(t, eng, e.match+` WITH b, [1, 2, 3, 4] AS {X} RETURN size({X}) AS v`, "x", `4`)
			bndCheck(t, eng, e.match+` WITH [1, 2, 3, 4] AS {X}, b RETURN {X}[1] AS v`, "x", `2`)
		})
	}
}

// TestRebindingBoundary_AggregationRebindsTheName is the #2878 matrix: an
// aggregate output, or a grouping key, re-binds a relationship or path name.
func TestRebindingBoundary_AggregationRebindsTheName(t *testing.T) {
	t.Parallel()
	eng := bndEngine(t)
	for _, e := range bndEarlier {
		for _, tc := range []struct{ shape, want string }{
			{`WITH count(*) AS {X} RETURN {X} AS v`, `1`},
			{`WITH count(*) AS {X} RETURN {X} + 1 AS v`, `2`},
			{`WITH count(*) AS {X}, 1 AS one RETURN {X} AS v`, `1`},
			{`WITH 1 AS one, count(*) AS {X} RETURN {X} AS v`, `1`},
			{`WITH b, count(*) AS {X} RETURN {X} AS v`, `1`},
			{`WITH count(x) AS {X} RETURN {X} AS v`, `1`},
			{`WITH collect(b.n) AS {X} RETURN {X} AS v`, `[2]`},
			{`WITH b.n AS {X}, count(*) AS c RETURN {X} AS v`, `2`},
			{`WITH count(*) AS {X} WHERE {X} = 1 RETURN {X} AS v`, `1`},
			{`RETURN count(*) AS v`, `1`},
		} {
			t.Run(e.kind, func(t *testing.T) {
				bndCheck(t, eng, e.match+` `+tc.shape, "x", tc.want)
			})
		}
		// No boundary between the binding and the aggregate: the alias is the
		// RETURN's own output column.
		t.Run(e.kind+"_return", func(t *testing.T) {
			for _, n := range []string{"x", "z"} {
				q := e.match + ` RETURN count(*) AS ` + n
				if got, err := bndColumn(eng, q, n); err != nil || got != `1` {
					t.Errorf("%s\n  got [%s], err %v, want [1]", q, got, err)
				}
			}
		})
	}
}

// TestRebindingBoundary_ProjectionCarriesTheVariable is the carried half of
// #2920: the variable forwarded beside other items, in every position, read by
// RETURN and by the WITH's own WHERE, and a NULL forwarded out of an OPTIONAL
// MATCH, which the stale coordinates turned into a fabricated relationship.
func TestRebindingBoundary_ProjectionCarriesTheVariable(t *testing.T) {
	t.Parallel()
	eng := bndEngine(t)
	for _, tc := range []struct {
		kind, match, read, want string
	}{
		{"relationship", `MATCH (a:A)-[x]->(b)`, `x.w`, `1`},
		{"relationship_type", `MATCH (a:A)-[x]->(b)`, `type(x)`, `"R"`},
		{"fixed_path", `MATCH x = (a:A)-->(b)`, `[n IN nodes(x) | n.n]`, `[1, 2]`},
		{"varlen_path", `MATCH x = (a:A)-[*1..1]->(b)`, `[n IN nodes(x) | n.n]`, `[1, 2]`},
		{"varlen_rel", `MATCH (a:A)-[x*1..1]->(b)`, `[r IN x | r.w]`, `[1]`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, with := range []string{
				`WITH x`,
				`WITH [0, 1, 2, 3] AS q, x`,
				`WITH x, [0, 1, 2, 3] AS q`,
				`WITH 0 AS y, x, [0, 1, 2, 3] AS q`,
				`WITH b, 7 AS k, x`,
				`WITH x, count(*) AS c`,
				`WITH [0, 1, 2, 3] AS q, x WITH 0 AS y, x`,
			} {
				bndCheck(t, eng, tc.match+` `+with+` RETURN `+tc.read+` AS v`, "x", tc.want)
				bndCheck(t, eng, tc.match+` `+with+` WHERE `+tc.read+` = `+tc.want+` RETURN `+tc.read+` AS v`, "x", tc.want)
			}
		})
	}
	for _, tc := range []struct{ kind, optional string }{
		{"relationship", `OPTIONAL MATCH (c)-[x]->()`},
		{"fixed_path", `OPTIONAL MATCH x = (c)-->()`},
		{"varlen_path", `OPTIONAL MATCH x = (c)-[*1..1]->()`},
		{"varlen_rel", `OPTIONAL MATCH (c)-[x*1..1]->()`},
	} {
		t.Run(tc.kind+"_null", func(t *testing.T) {
			for _, with := range []string{
				`WITH 0 AS y, 0 AS w, 0 AS k, x`,
				`WITH [0, 1, 2, 3] AS q, x`,
				`WITH x, 0 AS y, 0 AS w, 0 AS k`,
			} {
				q := `MATCH (c:C) ` + tc.optional + ` ` + with
				bndCheck(t, eng, q+` RETURN x AS v`, "x", `null`)
				bndCheck(t, eng, q+` RETURN x IS NULL AS v`, "x", `true`)
				bndCheck(t, eng, q+` WITH x WHERE x IS NULL RETURN 1 AS v`, "x", `1`)
			}
		})
	}
}

// TestRebindingBoundary_UnwindRebindsTheName pins #2877 for every earlier
// binding form: an UNWIND after the boundary re-binds the name to the element.
func TestRebindingBoundary_UnwindRebindsTheName(t *testing.T) {
	t.Parallel()
	eng := bndEngine(t)
	for _, e := range bndEarlier {
		t.Run(e.kind, func(t *testing.T) {
			bndCheck(t, eng, e.match+` WITH collect(b.n) AS ws UNWIND ws AS {X} RETURN {X} AS v`, "x", `2`)
			bndCheck(t, eng, e.match+` WITH b, [3, 4] AS ws UNWIND ws AS {X} RETURN {X} AS v`, "x", `3;4`)
			bndCheck(t, eng, e.match+` WITH x AS ws UNWIND [5] AS {X} RETURN {X} AS v`, "x", `5`)
		})
	}
}

// bndParallelEngine is two parallel (:X)-[:R]->(:Y) relationships and nothing
// else, the graph of the #2921 report.
func bndParallelEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ipExec(t, eng, `CREATE (a:X)-[:R {w: 1}]->(b:Y), (a)-[:R {w: 2}]->(b)`)
	return eng
}

// TestRebindingBoundary_UniquenessIsPerPattern is the #2921 matrix. A
// relationship variable of an earlier clause excludes a relationship from a
// later pattern only when that pattern names it again.
func TestRebindingBoundary_UniquenessIsPerPattern(t *testing.T) {
	t.Parallel()
	eng := bndParallelEngine(t)
	for _, tc := range []struct{ q, want string }{
		// Unrelated earlier variables: every pairing of the two edges.
		{`MATCH ()-[r]->() WITH r MATCH (a)-[:R*1..1]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() MATCH (a)-[:R*1..1]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[:R*1..2]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)<-[:R*1..1]-(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[:R*1..1]-(b) RETURN count(*) AS v`, `8`},
		{`MATCH (a)-[r]->() WITH a, r MATCH (a)-[:R*1..1]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH (a)-[r]->(b) WITH a, b, r MATCH (a)-[:R*1..1]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r OPTIONAL MATCH (a)-[:R*1..1]->(b) RETURN count(b) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[s:R]->(b) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[rs:R*1..1]->(b) RETURN count(*) AS v`, `4`},
		// A reused variable is part of the pattern and still excludes: r
		// bound either way round, then the other parallel edge only.
		{`MATCH ()-[r]->() WITH r MATCH (a)-[r]-(b)-[:R*1..1]-(c) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[r]-(b)-[s:R]-(c) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b)<-[:R*1..1]-(a) RETURN count(*) AS v`, `2`},
		// One earlier variable reused and one not: only the reused one
		// excludes, so the second edge — bound to the unrelated r2 — remains.
		// r1 and r2 are the two ordered pairs of distinct edges.
		{`MATCH ()-[r1]->() MATCH ()-[r2]->() WHERE r1 <> r2 WITH r1, r2 MATCH (a)-[r1]-(b)-[:R*1..1]-(c) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r1]->() MATCH ()-[r2]->() WHERE r1 <> r2 WITH r1, r2 MATCH (a)-[r1]-(b)-[s:R]-(c) RETURN count(*) AS v`, `4`},
		{`MATCH ()-[r1]->() MATCH ()-[r2]->() WHERE r1 <> r2 WITH r1, r2 MATCH (a)-[r1]-(b)-[:R*1..1]-(c) RETURN count(r2) AS v`, `4`},
		// Three relationships of one pattern over two edges: none.
		{`MATCH ()-[r1]->() MATCH ()-[r2]->() WHERE r1 <> r2 WITH r1, r2 MATCH (a)-[r1]-(b)-[:R*1..1]-(c)-[:R*1..1]-(d) RETURN count(*) AS v`, `0`},
		// Uniqueness inside ONE pattern is unchanged.
		{`MATCH (a)-[s]->(b)<-[:R*1..1]-(a) RETURN count(*) AS v`, `2`},
		{`MATCH (a)-[s]->(b), (a)-[:R*1..1]->(b) RETURN count(*) AS v`, `2`},
	} {
		got, err := bndRows(eng, tc.q)
		if err != nil {
			t.Errorf("%s\n  error: %v", tc.q, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s\n  got  [%s]\n  want [%s]", tc.q, got, tc.want)
		}
	}
}

// TestRebindingBoundary_WithWhereReadsTheAlias is the #2922 matrix. Each
// predicate holds for the one row, so it must keep it and its negation must
// drop it; and it must agree with the same predicate on a separate WITH, which
// sees the projected column rather than a replaced expression.
func TestRebindingBoundary_WithWhereReadsTheAlias(t *testing.T) {
	t.Parallel()
	eng := bndEngine(t)
	for _, tc := range []struct{ with, pred string }{
		{`WITH [1] AS q`, `q[0] = 1`},
		{`WITH [1, 2, 3] AS q`, `q[1..] = [2, 3]`},
		{`WITH [1, 2, 3] AS q`, `q[..1] = [1]`},
		{`WITH [1, 2, 3] AS q`, `q[size(q) - 1] = 3`},
		{`WITH [[1, 2]] AS q`, `q[0][1] = 2`},
		{`WITH [[1, 2]] AS q`, `q[0][0..1] = [1]`},
		{`WITH {k: 1} AS q`, `q['k'] = 1`},
		{`WITH {k: [4]} AS q`, `q.k[0] = 4`},
		{`MATCH (n:A) WITH [n] AS q`, `(q[0]).n = 1`},
		{`MATCH (n:A) WITH [n] AS q`, `q[0].n = 1`},
		{`MATCH (n:A) WITH [n] AS q`, `q[0]:A`},
		{`MATCH (n:A) WITH [[n]] AS q`, `q[0][0]:A`},
		{`MATCH (n:A) WITH [n] AS q`, `[m IN q | m.n][0] = 1`},
		{`WITH [1] AS q`, `[q[0]] = [1]`},
		{`WITH [1] AS q`, `{k: q[0]}.k = 1`},
		{`WITH [1] AS q`, `CASE WHEN q[0] = 1 THEN true ELSE false END`},
		{`WITH [1] AS q`, `CASE q[0] WHEN 1 THEN true END`},
		{`WITH [1] AS q`, `[x IN q | x + 1] = [2]`},
		{`WITH [1] AS q`, `[x IN q WHERE x = 1] = [1]`},
		{`WITH [1] AS q`, `any(x IN q WHERE x = q[0])`},
		{`WITH [1] AS q`, `reduce(s = 0, x IN q | s + x + q[0]) = 2`},
		{`WITH [1, 2] AS q`, `[q IN [5] | q] = [5]`},
		{`WITH [1, 2] AS q`, `reduce(q = 0, x IN [3] | q + x) = 3`},
		{`MATCH (x:A) WITH x.n AS q`, `[x IN [7] | q] = [1]`},
		{`MATCH (x:A) WITH x.n AS q`, `any(x IN [1] WHERE x = q)`},
		{`MATCH (x:A) WITH x.n AS q`, `reduce(x = 0, y IN [1] | x + q) = 1`},
		{`MATCH (x:A) WITH x.n AS q, x`, `[x IN [1] | x + q] = [2]`},
		{`MATCH (n:A) WITH n AS m, [3] AS q`, `m {.n, q}.q = [3]`},
		{`MATCH (n:A) WITH n AS m`, `m {.n}.n = 1`},
		{`MATCH (n:A) WITH n AS m, [3] AS q`, `m {k: q[0]}.k = 3`},
	} {
		names := "q"
		if strings.Contains(tc.with, "AS m") {
			names = "m"
			if strings.Contains(tc.with, "AS q") {
				names = "m, q"
			}
		} else if strings.Contains(tc.with, "AS q, x") {
			names = "q, x"
		}
		for _, c := range []struct{ q, want string }{
			{tc.with + ` WHERE ` + tc.pred + ` RETURN count(*) AS v`, `1`},
			{tc.with + ` WHERE NOT (` + tc.pred + `) RETURN count(*) AS v`, `0`},
			{tc.with + ` WITH ` + names + ` WHERE ` + tc.pred + ` RETURN count(*) AS v`, `1`},
		} {
			got, err := bndRows(eng, c.q)
			if err != nil {
				t.Errorf("%s\n  error: %v", c.q, err)
				continue
			}
			if got != c.want {
				t.Errorf("%s\n  got  [%s]\n  want [%s]", c.q, got, c.want)
			}
		}
	}
}
