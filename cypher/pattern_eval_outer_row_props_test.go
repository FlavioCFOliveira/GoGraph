package cypher_test

// pattern_eval_outer_row_props_test.go — rmp #2913 and #2911.
//
//   - #2913. The expression-level pattern evaluator (the WHERE pattern predicate
//     and the unhoisted pattern comprehension) evaluated a NODE property map
//     against an empty row with no function registry, so a value that read an
//     outer variable — `WHERE (a)-->(:N {k: x.k})` — was NULL and matched
//     nothing, and a function call failed and was swallowed as a non-match. A
//     bare WHERE predicate also received no function registry at all, so a
//     function call inside a RELATIONSHIP property map failed the query with "no
//     function registry". A property map constrains the element exactly as the
//     equivalent WHERE predicates over the element would
//     (clauses/match/Match2.feature [5], "Match relationship with inline property
//     value"; Match1.feature [4] for a node), and those predicates see the whole
//     row, parameters and functions.
//   - #2911. A bare pattern predicate naming a variable statically bound to a
//     non-relationship (or non-node) value compiled, while MATCH and the pattern
//     comprehension over the same pattern are rejected at compile time with
//     VariableTypeConflict (clauses/match/Match2.feature [13] "Fail when matching
//     a relationship variable bound to a value", Match1.feature [11] for a node).
//
// Every #2913 route is asserted against the MATCH baseline on the same pattern
// and graph, and the baseline itself against absolute answers on the shapes that
// decide the defect.
//
// Layer: short. goleak-clean (engine and graph are local).

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// orCase is one pattern probed with a and x bound to every pair of :N nodes.
type orCase struct {
	name    string
	pattern string
}

var orCases = []orCase{
	// Node property maps reading the outer row.
	{"end_outer_prop", `(a)-->(:N {k: x.k})`},
	{"end_outer_prop_in", `(a)<--(:N {k: x.k})`},
	{"end_outer_prop_undirected", `(a)--(:N {k: x.k})`},
	{"start_outer_prop", `(:N {k: x.k})-->(a)`},
	{"middle_outer_prop", `(a)-->(:N {k: x.k})-->()`},
	{"end_outer_null", `(a)-->(:N {k: x.missing})`},
	{"end_outer_and_literal", `(a)-->(:N {k: x.k, k2: 1})`},
	{"end_outer_expression", `(a)-->({k: toLower(toUpper(x.k))})`},
	{"end_function_literal", `(a)-[:R]->(:N {k: toLower('B')})`},
	{"varlen_end_outer_prop", `(a)-[:R*1..2]->(:N {k: x.k})`},
	{"varlen_then_outer_prop", `(a)-[:R*0..1]->()-[:R]->(:N {k: x.k})`},
	{"two_hop_outer_props", `(a)-[:R]->(:N {k: x.k})<-[:R]-(:N {k: a.k})`},
	// Relationship property maps calling functions and reading the outer row.
	{"rel_function", `(a)-[:R {w: toInteger('1')}]->()`},
	{"rel_function_abs", `(a)-[:R {w: abs(-2)}]->(x)`},
	{"rel_outer_function", `(a)-[:R {w: size(x.k)}]->()`},
	{"varlen_rel_function", `(a)-[:R*1..2 {w: toInteger('1')}]->(x)`},
}

// TestPatternPredicate_PropsReadOuterRow_MatchesMatch asserts, for every fixture
// × case, that each consumer route yields exactly the bag MATCH yields (#2913).
func TestPatternPredicate_PropsReadOuterRow_MatchesMatch(t *testing.T) {
	t.Parallel()
	const outer = `MATCH (a:N), (x:N)`
	const keys = `a.k AS ka, x.k AS kx`
	cols := []string{"ka", "kx"}
	for _, fx := range rbFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := rbEngine(t, fx)
			for _, c := range orCases {
				t.Run(c.name, func(t *testing.T) {
					distinct := rbRows(t, eng, outer+` MATCH `+c.pattern+` RETURN DISTINCT `+keys, cols...)
					counted := rbRows(t, eng, outer+` MATCH `+c.pattern+` RETURN `+keys+`, count(*) AS n`, append(cols, "n")...)
					for _, want := range [][]string{distinct, counted} {
						if len(want) == 1 && strings.HasPrefix(want[0], "ERROR") {
							t.Fatalf("MATCH baseline failed: %v", want)
						}
					}
					routes := []struct {
						name  string
						query string
						cols  []string
						want  []string
					}{
						{"where_predicate", outer + ` WHERE ` + c.pattern + ` RETURN ` + keys, cols, distinct},
						{"where_not_predicate", outer + ` WHERE NOT ` + c.pattern + ` RETURN ` + keys, cols,
							rbComplement(rbRows(t, eng, outer+` RETURN `+keys, cols...), distinct)},
						{"exists_subquery", outer + ` WHERE EXISTS { ` + c.pattern + ` } RETURN ` + keys, cols, distinct},
						{"comprehension_return", outer + ` RETURN ` + keys + `, size([` + c.pattern + ` | 1]) AS n`,
							append(cols, "n"), counted},
						{"comprehension_with_where", outer + ` WITH a, x, size([` + c.pattern + ` | 1]) AS n ` +
							`WHERE n > 0 RETURN ` + keys, cols, distinct},
						// UNWIND is not hoisted: the expression-level evaluator runs it.
						{"comprehension_unwind", outer + ` UNWIND [` + c.pattern + ` | 1] AS y RETURN ` + keys + `, count(y) AS n`,
							append(cols, "n"), counted},
					}
					for _, r := range routes {
						got := rbRows(t, eng, r.query, r.cols...)
						if r.name == "comprehension_return" {
							got = slices.DeleteFunc(got, func(s string) bool { return strings.HasSuffix(s, "|0") })
						}
						if strings.Join(got, ";") != strings.Join(r.want, ";") {
							t.Errorf("%s: %q\n  got  %v\n  want %v (MATCH baseline)", r.name, r.query, got, r.want)
						}
					}
				})
			}
		})
	}
}

// rbComplement returns the rows of all that are not in minus; both are sorted
// and distinct.
func rbComplement(all, minus []string) []string {
	var out []string
	for _, r := range all {
		if !slices.Contains(minus, r) {
			out = append(out, r)
		}
	}
	return out
}

// TestPatternPredicate_PropsReadOuterRow_WriteRoute asserts the same parity on
// a statement that writes, whose pattern evaluator is built by a different
// scaffold ([writeEvalScaffold]) than a read-only statement's (#2913).
func TestPatternPredicate_PropsReadOuterRow_WriteRoute(t *testing.T) {
	t.Parallel()
	fx := rbFixtures[1] // parallel_diff_props
	for _, c := range orCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := rbEngine(t, fx)
			want := rbRows(t, eng, `MATCH (a:N), (x:N) MATCH `+c.pattern+` RETURN DISTINCT a.k AS ka, x.k AS kx`, "ka", "kx")
			for _, q := range []string{
				`MATCH (a:N), (x:N) CREATE (:Tmp) WITH DISTINCT a, x WHERE ` + c.pattern + ` RETURN a.k AS ka, x.k AS kx`,
				`MATCH (a:N), (x:N) CREATE (:Tmp) WITH DISTINCT a, x UNWIND [` + c.pattern + ` | 1] AS y ` +
					`RETURN DISTINCT a.k AS ka, x.k AS kx`,
			} {
				res, err := eng.RunInTx(context.Background(), q, rbParams)
				if err != nil {
					t.Fatalf("RunInTx(%q): %v", q, err)
				}
				var got []string
				for res.Next() {
					rec := res.Record()
					got = append(got, fmt.Sprintf("%v|%v", rec["ka"], rec["kx"]))
				}
				rerr := res.Err()
				if cerr := res.Close(); cerr != nil && rerr == nil {
					rerr = cerr
				}
				if rerr != nil {
					t.Fatalf("%q: %v", q, rerr)
				}
				sort.Strings(got)
				if strings.Join(got, ";") != strings.Join(want, ";") {
					t.Errorf("%q\n  got  %v\n  want %v (MATCH baseline)", q, got, want)
				}
			}
		})
	}
}

// TestPatternPredicate_PropsReadOuterRow_MatchBaselineAbsolute pins the MATCH
// baseline on the shapes that decide #2913.
func TestPatternPredicate_PropsReadOuterRow_MatchBaselineAbsolute(t *testing.T) {
	t.Parallel()
	eng := rbEngine(t, rbFixtures[1]) // parallel_diff_props: a -R{w:1}-> b, a -R{w:2}-> b
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`MATCH (a:N), (x:N) MATCH (a)-->(:N {k: x.k}) RETURN DISTINCT a.k + x.k AS k`, []string{`"ab"`}},
		{`MATCH (a:N), (x:N) MATCH (:N {k: x.k})-->(a) RETURN DISTINCT a.k + x.k AS k`, []string{`"ba"`}},
		{`MATCH (a:N) MATCH (a)-[:R]->(:N {k: toLower('B')}) RETURN a.k AS k`, []string{`"a"`, `"a"`}},
		{`MATCH (a:N) MATCH (a)-[:R {w: toInteger('1')}]->() RETURN a.k AS k`, []string{`"a"`}},
		{`MATCH (a:N), (x:N) MATCH (a)-->(:N {k: x.missing}) RETURN a.k AS k`, nil},
	} {
		if got := rbRows(t, eng, tc.query, "k"); strings.Join(got, ";") != strings.Join(tc.want, ";") {
			t.Errorf("%q: got %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestPatternPredicate_BoundVariableTypeConflict asserts that a bare pattern
// predicate naming a variable statically bound to something that cannot fill
// its position is rejected at compile time with VariableTypeConflict, as MATCH
// and the pattern comprehension over the same pattern are (#2911), and that a
// binding whose type is known only at runtime is still left to the runtime.
func TestPatternPredicate_BoundVariableTypeConflict(t *testing.T) {
	t.Parallel()
	eng := rbEngine(t, rbFixtures[1])
	run := func(q string) error {
		res, err := eng.Run(context.Background(), q, rbParams)
		if err != nil {
			return err
		}
		for res.Next() {
		}
		rerr := res.Err()
		if cerr := res.Close(); cerr != nil && rerr == nil {
			rerr = cerr
		}
		return rerr
	}
	// The values of Match2.feature [13] / Match1.feature [11].
	invalid := []string{`true`, `123`, `123.4`, `'foo'`, `[]`, `[10]`, `{x: 1}`, `{x: []}`}
	conflicts := make([]string, 0, 25*len(invalid)+8+4)
	for _, v := range invalid {
		for _, pattern := range []string{`(a)-[r]->(b)`, `(a)<-[r]-(b)`, `(a)-[r]-(b)`, `(a)-[:R]->()-[r]->(b)`} {
			conflicts = append(conflicts,
				`WITH `+v+` AS r MATCH (a), (b) WHERE `+pattern+` RETURN a`,
				`WITH `+v+` AS r MATCH (a), (b) WHERE NOT `+pattern+` RETURN a`,
				`WITH `+v+` AS r MATCH (a), (b) RETURN [`+pattern+` | 1] AS l`,
				`WITH `+v+` AS r MATCH (a), (b) MATCH `+pattern+` RETURN a`)
		}
		for _, pattern := range []string{`(n)-->(b)`, `(b)-->(n)`, `(b)-->()-->(n)`} {
			conflicts = append(conflicts,
				`WITH `+v+` AS n MATCH (b) WHERE `+pattern+` RETURN b`,
				`WITH `+v+` AS n MATCH (b) RETURN [`+pattern+` | 1] AS l`,
				`WITH `+v+` AS n MATCH `+pattern+` RETURN b`)
		}
	}
	// A variable-length hop accepts a list, but not a scalar or a map.
	for _, v := range []string{`true`, `123`, `'foo'`, `{x: 1}`} {
		conflicts = append(conflicts,
			`WITH `+v+` AS rs MATCH (a), (b) WHERE (a)-[rs*]->(b) RETURN a`,
			`WITH `+v+` AS rs MATCH (a), (b) MATCH (a)-[rs*]->(b) RETURN a`)
	}
	// A node variable in a relationship position and the reverse.
	conflicts = append(conflicts,
		`MATCH (a)-[r]->(b) WHERE (r)-->(b) RETURN a`,
		`MATCH (a)-[r]->(b) WHERE (a)-[b]->() RETURN a`,
		`MATCH (a)-[r]->(b) RETURN [(r)-->(b) | 1] AS l`,
		`MATCH (a)-[r]->(b) RETURN [(a)-[b]->() | 1] AS l`)
	for _, q := range conflicts {
		err := run(q)
		if err == nil || !strings.Contains(err.Error(), "VariableTypeConflict") {
			t.Errorf("%q: got %v, want a VariableTypeConflict", q, err)
		}
	}
	// A binding whose static type is unknown — NULL, a parameter, a projected
	// relationship or a list for a variable-length hop — compiles.
	for _, q := range []string{
		`WITH null AS r MATCH (a), (b) WHERE (a)-[r]->(b) RETURN a`,
		`WITH $notRel AS r MATCH (a), (b) WHERE (a)-[r]->(b) RETURN a`,
		`MATCH ()-[r]->() WITH r MATCH (a), (b) WHERE (a)-[r]->(b) RETURN a`,
		`MATCH ()-[r]->() WITH collect(r) AS rs MATCH (a), (b) WHERE (a)-[rs*]->(b) RETURN a`,
		`WITH [] AS rs MATCH (a), (b) WHERE (a)-[rs*]->(b) RETURN a`,
		`WITH null AS n MATCH (b) WHERE (n)-->(b) RETURN b`,
		`WITH $notRel AS n MATCH (b) WHERE (n)-->(b) RETURN b`,
		`MATCH (a)-[r]->(b) WHERE (a)-[r]->(b) RETURN a`,
	} {
		if err := run(q); err != nil {
			t.Errorf("%q: %v, want no error", q, err)
		}
	}
}
