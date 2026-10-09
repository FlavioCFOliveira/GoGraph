package cypher_test

// pattern_eval_rel_binding_test.go — rmp #2905 and #2908.
//
// Two constraints a relationship pattern places on the ONE relationship that
// fills it, which the expression-level pattern evaluator (the WHERE pattern
// predicate and the unhoisted pattern comprehension) used to ignore:
//
//   - #2905, a bound relationship variable. `WITH r … WHERE (a)-[r]->(b)` holds
//     only when r itself runs from a to b: a variable already in scope denotes
//     one relationship, exactly as `MATCH (a)-[r]->(b)` with r bound does
//     (clauses/match/Match4.feature [7] binds r in an earlier MATCH and counts 32
//     paths through that one relationship; Match2.feature [13] rejects a
//     variable bound to a non-relationship value).
//   - #2908, a relationship property map. `[:R {w: 1}]` matches only a
//     relationship whose w equals 1 (clauses/match/Match2.feature [5], "Match
//     relationship with inline property value"), tested on the relationship
//     itself — two parallel relationships between one pair may carry different
//     properties — and for a variable-length hop on every relationship of the
//     path.
//
// Every route is asserted against the MATCH baseline on the same pattern and
// graph, and the baseline itself against absolute answers on the shapes that
// decide each defect, so the routes cannot agree on a wrong answer.
//
// Layer: short. goleak-clean (engine and graph are local).

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// rbFixture is one graph over (:N {k:'a'}), (:N {k:'b'}), (:N {k:'c'}).
type rbFixture struct {
	name  string
	edges []string // "a R b 1" = (a)-[:R {w: 1}]->(b); the w field is Cypher text
}

var rbFixtures = []rbFixture{
	{"single", []string{"a R b 1"}},
	{"parallel_diff_props", []string{"a R b 1", "a R b 2"}},
	{"parallel_mixed_types", []string{"a R b 1", "a S b 2"}},
	{"two_cycle", []string{"a R b 1", "b R a 2"}},
	{"self_loop", []string{"a R a 1"}},
	{"self_loop_and_edge", []string{"a R a 2", "a R b 1"}},
	// The shortcut a → c carries w = 2, so `[:R* {w: 1}]` reaches c only by the
	// length-2 path a → b → c.
	{"chain_with_shortcut", []string{"a R b 1", "b R c 1", "a R c 2"}},
	{"directed_triangle", []string{"a R b 1", "b R c 1", "c R a 1"}},
	// A string value is hoisted onto an auto-parameter by StripLiterals, so it
	// reaches the evaluator as a parameter reference (rmp #2507).
	{"string_props", []string{"a R b 'x'", "a R b 1", "b R c 'x'"}},
}

// rbParams is passed to every query: $w for the parameterised map value and
// $nw for a NULL one.
var rbParams = map[string]expr.Value{"w": expr.IntegerValue(2), "nw": expr.Null, "notRel": expr.IntegerValue(7)}

func rbEngine(t *testing.T, fx rbFixture) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	qs := make([]string, 0, 1+len(fx.edges))
	qs = append(qs, `CREATE (:N {k:'a'}), (:N {k:'b'}), (:N {k:'c'})`)
	for _, e := range fx.edges {
		f := strings.Fields(e)
		qs = append(qs, fmt.Sprintf(`MATCH (x:N {k:'%s'}), (y:N {k:'%s'}) CREATE (x)-[:%s {w: %s}]->(y)`, f[0], f[2], f[1], f[3]))
	}
	for _, q := range qs {
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
	return eng
}

// rbRows runs query with rbParams and returns its rows rendered as "c1|c2|…"
// over cols, sorted. An error is returned as the single row "ERROR: …" so a
// route that fails to run is reported as a mismatch rather than aborting.
func rbRows(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, rbParams)
	if err != nil {
		return []string{"ERROR: " + err.Error()}
	}
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf("%v", rec[c])
		}
		out = append(out, strings.Join(parts, "|"))
	}
	rerr := res.Err()
	if cerr := res.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return []string{"ERROR: " + rerr.Error()}
	}
	sort.Strings(out)
	return out
}

// rbCase is one pattern probed under one outer binding. outer binds every
// variable the pattern names and ends in a clause a WHERE may follow; keys
// projects those variables, and cols names the projected columns.
type rbCase struct {
	name    string
	outer   string
	withVar string
	keys    string
	cols    []string
	pattern string
}

// rbBoundRel binds r to each relationship of the graph in turn, and a and b to
// every pair of nodes (#2905).
func rbBoundRel(name, pattern string) rbCase {
	return rbCase{
		name:    "bound_rel/" + name,
		outer:   `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N)`,
		withVar: `r, a, b`,
		keys:    `id(r) AS ir, a.k AS ka, b.k AS kb`,
		cols:    []string{"ir", "ka", "kb"},
		pattern: pattern,
	}
}

// rbProps binds a and b to every pair of nodes (#2908).
func rbProps(name, pattern string) rbCase {
	return rbCase{
		name:    "props/" + name,
		outer:   `MATCH (a:N), (b:N)`,
		withVar: `a, b`,
		keys:    `a.k AS ka, b.k AS kb`,
		cols:    []string{"ka", "kb"},
		pattern: pattern,
	}
}

// rbPropsOneEnd binds a alone, for a pattern whose far end is anonymous or a.
func rbPropsOneEnd(name, pattern string) rbCase {
	return rbCase{
		name:    "props/" + name,
		outer:   `MATCH (a:N)`,
		withVar: `a`,
		keys:    `a.k AS ka`,
		cols:    []string{"ka"},
		pattern: pattern,
	}
}

var rbCases = []rbCase{
	// #2905: a bound relationship variable on a fixed hop.
	rbBoundRel("out", `(a)-[r]->(b)`),
	rbBoundRel("in", `(a)<-[r]-(b)`),
	rbBoundRel("undirected", `(a)-[r]-(b)`),
	rbBoundRel("typed_match", `(a)-[r:R]->(b)`),
	rbBoundRel("typed_mismatch", `(a)-[r:S]->(b)`),
	rbBoundRel("typed_alternatives", `(a)-[r:S|R]-(b)`),
	rbBoundRel("self_undirected", `(a)-[r]-(a)`),
	rbBoundRel("self_in", `(a)<-[r]-(a)`),
	rbBoundRel("with_props", `(a)-[r {w: 1}]->(b)`),
	rbBoundRel("with_param_props", `(a)-[r {w: $w}]-(b)`),
	rbBoundRel("hop_then", `(a)-[:R]->()-[r]->(b)`),
	// Relationship isomorphism: the other hop may not re-use r, whichever hop
	// r fills (rmp #2909: MATCH, and so EXISTS { } and the hoisted comprehension,
	// used to let a later hop re-use r when r was the FIRST hop).
	rbBoundRel("cycle_bound_second", `(a)-[:R]-(b)-[r]-(a)`),
	rbBoundRel("parallel_bound_second", `(a)-[:R]->(b)<-[r]-(a)`),
	rbBoundRel("then_hop", `(a)-[r]->(b)-[:R]->()`),
	rbBoundRel("cycle_bound_first", `(a)-[r]-(b)-[:R]-(a)`),
	rbBoundRel("parallel_other_hop", `(a)-[r]->(b)<-[:R]-(a)`),
	rbBoundRel("parallel_untyped_other_hop", `(a)-[r]->(b)<--(a)`),
	rbBoundRel("anon_middle", `(a)-[r]-()-[:R]-(b)`),
	rbBoundRel("bound_middle", `(a)-[:R]-()-[r]-()-[:R]-(b)`),
	rbBoundRel("then_varlen", `(a)-[r]->()-[:R*0..2]->(b)`),
	rbBoundRel("varlen_then", `(a)-[:R*0..1]-()-[r]-(b)`),

	// #2908: a relationship property map, fixed hops.
	rbProps("out_w1", `(a)-[:R {w: 1}]->(b)`),
	rbProps("out_w2", `(a)-[:R {w: 2}]->(b)`),
	rbProps("out_absent_value", `(a)-[:R {w: 9}]->(b)`),
	rbProps("out_absent_key", `(a)-[:R {v: 1}]->(b)`),
	rbProps("out_two_keys", `(a)-[:R {w: 1, v: 1}]->(b)`),
	rbProps("in_w1", `(a)<-[:R {w: 1}]-(b)`),
	rbProps("undirected_w2", `(a)-[:R {w: 2}]-(b)`),
	rbProps("untyped_w2", `(a)-[{w: 2}]->(b)`),
	rbProps("alternatives_w2", `(a)-[:R|S {w: 2}]->(b)`),
	rbProps("string", `(a)-[:R {w: 'x'}]->(b)`),
	rbProps("param", `(a)-[:R {w: $w}]->(b)`),
	rbProps("param_null", `(a)-[:R {w: $nw}]->(b)`),
	rbProps("literal_null", `(a)-[:R {w: null}]->(b)`),
	rbProps("expression", `(a)-[:R {w: 1 + 1}]->(b)`),
	rbProps("two_hop", `(a)-[:R {w: 1}]->()-[:R {w: 1}]->(b)`),
	rbProps("two_hop_mixed", `(a)-[:R {w: 1}]->()-[:R {w: 2}]->(b)`),
	rbProps("parallel_pair", `(a)-[:R {w: 1}]->(b)<-[:R {w: 2}]-(a)`),
	rbProps("parallel_pair_same", `(a)-[:R {w: 1}]->(b)<-[:R {w: 1}]-(a)`),
	rbPropsOneEnd("self_w1", `(a)-[:R {w: 1}]-(a)`),
	rbPropsOneEnd("self_w2_in", `(a)<-[:R {w: 2}]-(a)`),
	rbPropsOneEnd("anon_in_w2", `(a)<-[:R {w: 2}]-()`),
	rbPropsOneEnd("anon_undirected_w1", `(a)-[:R {w: 1}]-()`),

	// #2908: a relationship property map on a variable-length hop.
	rbProps("varlen_out_w1", `(a)-[:R* {w: 1}]->(b)`),
	rbProps("varlen_out_w2", `(a)-[:R* {w: 2}]->(b)`),
	rbProps("varlen_exact2_w1", `(a)-[:R*2 {w: 1}]->(b)`),
	rbProps("varlen_in_w1", `(a)<-[:R*1..2 {w: 1}]-(b)`),
	rbProps("varlen_undirected_w1", `(a)-[:R*1..2 {w: 1}]-(b)`),
	rbProps("varlen_zero_w1", `(a)-[:R*0..1 {w: 1}]->(b)`),
	rbProps("varlen_param", `(a)-[:R* {w: $w}]-(b)`),
	rbProps("varlen_string", `(a)-[:R*1..2 {w: 'x'}]->(b)`),
	rbProps("varlen_then_fixed", `(a)-[:R*1..2 {w: 1}]->()-[:R {w: 2}]->(b)`),
	rbPropsOneEnd("varlen_cycle_w1", `(a)-[:R* {w: 1}]->(a)`),
	rbPropsOneEnd("varlen_cycle_w2_in", `(a)<-[:R* {w: 2}]-(a)`),
	rbPropsOneEnd("varlen_undirected_cycle_w1", `(a)-[:R*2..3 {w: 1}]-(a)`),
}

// TestPatternPredicate_RelBindingAndProps_MatchesMatch asserts, for every
// fixture × case, that each consumer route yields exactly the bag MATCH yields.
func TestPatternPredicate_RelBindingAndProps_MatchesMatch(t *testing.T) {
	t.Parallel()
	for _, fx := range rbFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := rbEngine(t, fx)
			for _, c := range rbCases {
				t.Run(c.name, func(t *testing.T) {
					distinct := rbRows(t, eng, c.outer+` MATCH `+c.pattern+` RETURN DISTINCT `+c.keys, c.cols...)
					counted := rbRows(t, eng, c.outer+` MATCH `+c.pattern+` RETURN `+c.keys+`, count(*) AS n`, append(c.cols, "n")...)
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
						{"where_predicate", c.outer + ` WHERE ` + c.pattern + ` RETURN ` + c.keys, c.cols, distinct},
						{"exists_subquery", c.outer + ` WHERE EXISTS { ` + c.pattern + ` } RETURN ` + c.keys, c.cols, distinct},
						{"comprehension_return", c.outer + ` RETURN ` + c.keys + `, size([` + c.pattern + ` | 1]) AS n`,
							append(c.cols, "n"), counted},
						{"comprehension_with_where", c.outer + ` WITH ` + c.withVar + `, size([` + c.pattern + ` | 1]) AS n ` +
							`WHERE n > 0 RETURN ` + c.keys, c.cols, distinct},
						// UNWIND is not hoisted: the expression-level evaluator runs it.
						{"comprehension_unwind", c.outer + ` UNWIND [` + c.pattern + ` | 1] AS x RETURN ` + c.keys + `, count(x) AS n`,
							append(c.cols, "n"), counted},
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

// TestPatternComprehension_RelPropsBindsTheInstance asserts that a
// comprehension that introduces its relationship variable binds exactly the
// relationships whose own properties satisfy the map, per parallel instance,
// on both the hoisted and the expression-level route (#2908).
func TestPatternComprehension_RelPropsBindsTheInstance(t *testing.T) {
	t.Parallel()
	for _, fx := range rbFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := rbEngine(t, fx)
			for _, pattern := range []string{
				`(a)-[r:R {w: 1}]->(b)`,
				`(a)-[r {w: 2}]-(b)`,
				`(a)<-[r:R {w: $w}]-(b)`,
				`(a)-[r:R {w: 'x'}]->(b)`,
			} {
				t.Run(pattern, func(t *testing.T) {
					want := rbRows(t, eng, `MATCH `+pattern+` WHERE a:N RETURN a.k AS ka, id(r) AS ir`, "ka", "ir")
					for _, q := range []string{
						`MATCH (a:N) UNWIND [` + pattern + ` | id(r)] AS ir RETURN a.k AS ka, ir`,
						`MATCH (a:N) WITH a, [` + pattern + ` | id(r)] AS l UNWIND l AS ir RETURN a.k AS ka, ir`,
					} {
						got := rbRows(t, eng, q, "ka", "ir")
						if strings.Join(got, ";") != strings.Join(want, ";") {
							t.Errorf("%q\n  got  %v\n  want %v (MATCH baseline)", q, got, want)
						}
					}
				})
			}
		})
	}
}

// TestPatternPredicate_BoundRelNonRelationship asserts that a bound
// relationship variable holding NULL or a value that is not a relationship fills
// no hop, as MATCH answers (#2905).
func TestPatternPredicate_BoundRelNonRelationship(t *testing.T) {
	t.Parallel()
	eng := rbEngine(t, rbFixtures[1]) // parallel_diff_props
	for _, binding := range []string{`null`, `$nw`, `$notRel`} {
		for _, pattern := range []string{`(a)-[r]->(b)`, `(a)<-[r]-(b)`, `(a)-[r]-(b)`, `(a)-[r]->(b)-[]-()`} {
			q := `WITH ` + binding + ` AS r MATCH (a:N), (b:N)`
			for _, route := range []string{
				q + ` WHERE ` + pattern + ` RETURN a.k AS ka`,
				q + ` UNWIND [` + pattern + ` | 1] AS x RETURN a.k AS ka`,
				q + ` WITH a, b, r, size([` + pattern + ` | 1]) AS n WHERE n > 0 RETURN a.k AS ka`,
			} {
				if got := rbRows(t, eng, route, "ka"); len(got) != 0 {
					t.Errorf("%q: got %v, want no rows", route, got)
				}
			}
		}
	}
}

// TestPatternPredicate_RelBindingAndProps_MatchBaselineAbsolute pins the MATCH
// baseline the parity tests compare against, on the shapes that decide each
// defect.
func TestPatternPredicate_RelBindingAndProps_MatchBaselineAbsolute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		query   string
		want    []string
	}{
		// r bound to each of the two parallel a → b relationships: each fills
		// (a)-[r]->(b) once, and nothing else.
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)-[r]->(b) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		// The reverse direction is b ← a.
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)<-[r]-(b) RETURN a.k + b.k AS k`,
			[]string{`"ba"`, `"ba"`}},
		// Each parallel relationship completes the pair with the OTHER one, never
		// with itself, whichever hop r fills and whether the other hop is in the
		// same path or a later comma-separated one (rmp #2909: with r FIRST, MATCH
		// answered four rows).
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)-[:R]->(b)<-[r]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b)<--(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r OPTIONAL MATCH (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b), (b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (b)<-[:R]-(a), (a)-[r]->(b) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH (a:N {k: 'a'}) MATCH (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"parallel_diff_props", `MATCH ()-[r]->() WITH r MATCH p = (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`}},
		{"single", `MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b)<-[:R]-(a) RETURN a.k + b.k AS k`, nil},
		{"two_cycle", `MATCH ()-[r]->() WITH r MATCH (a)-[r]-(b)-[:R]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`, `"ba"`, `"ba"`}},
		{"self_loop", `MATCH ()-[r]->() WITH r MATCH (a)-[r]-(b)-[:R]-(a) RETURN a.k + b.k AS k`, nil},
		{"two_cycle", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)-[:R]->(b)<-[r]-(a) RETURN a.k + b.k AS k`, nil},
		{"self_loop", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH ()<-[:R]-(b)<-[r]-(a) RETURN a.k + b.k AS k`, nil},
		{"two_cycle", `MATCH ()-[r]->() WITH r MATCH (a:N), (b:N) MATCH (a)-[:R]-(b)-[r]-(a) RETURN a.k + b.k AS k`,
			[]string{`"ab"`, `"ab"`, `"ba"`, `"ba"`}},
		// A self-loop is crossed once by an undirected hop.
		{"self_loop", `MATCH ()-[r]->() WITH r MATCH (a:N) MATCH (a)-[r]-(a) RETURN a.k AS k`, []string{`"a"`}},
		{"parallel_diff_props", `MATCH (a:N)-[:R {w: 2}]->(b) RETURN a.k + b.k AS k`, []string{`"ab"`}},
		{"parallel_diff_props", `MATCH (a:N)-[:R {w: 9}]->(b) RETURN a.k + b.k AS k`, nil},
		{"parallel_diff_props", `MATCH (a:N)-[:R {w: 1}]->(b)<-[:R {w: 2}]-(a) RETURN a.k + b.k AS k`, []string{`"ab"`}},
		{"parallel_diff_props", `MATCH (a:N)-[:R {w: 1}]->(b)<-[:R {w: 1}]-(a) RETURN a.k + b.k AS k`, nil},
		// The w = 2 shortcut does not count; the w = 1 chain does.
		{"chain_with_shortcut", `MATCH (a:N {k: 'a'})-[:R* {w: 1}]->(b) RETURN b.k AS k`, []string{`"b"`, `"c"`}},
		{"chain_with_shortcut", `MATCH (a:N {k: 'a'})-[:R* {w: 2}]->(b) RETURN b.k AS k`, []string{`"c"`}},
		{"self_loop_and_edge", `MATCH (a:N)-[:R* {w: 1}]->(a) RETURN a.k AS k`, nil},
		{"string_props", `MATCH (a:N)-[:R*1..2 {w: 'x'}]->(b) RETURN a.k + b.k AS k`, []string{`"ab"`, `"ac"`, `"bc"`}},
	} {
		t.Run(tc.fixture+"/"+tc.query, func(t *testing.T) {
			t.Parallel()
			var fx rbFixture
			for _, f := range rbFixtures {
				if f.name == tc.fixture {
					fx = f
				}
			}
			got := rbRows(t, rbEngine(t, fx), tc.query, "k")
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
