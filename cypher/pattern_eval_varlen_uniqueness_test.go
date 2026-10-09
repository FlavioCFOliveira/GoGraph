package cypher_test

// pattern_eval_varlen_uniqueness_test.go — rmp #2898.
//
// A variable-length relationship pattern matches every PATH whose length lies
// in its range, not every node reachable at its shortest distance, and the
// relationships it crosses take part in the relationship isomorphism of the
// whole pattern (openCypher 9, "Uniqueness" of MATCH: one relationship may fill
// at most one relationship slot of a pattern, a variable-length slot included).
// The TCK pins both for MATCH: clauses/match/Match4.feature [7] (a
// variable-length step may not re-use the relationship bound to r in the same
// pattern), Match5.feature [8]-[10] (a *k..k range yields the paths of exactly
// length k), Match4.feature [3] (*0..1 in the middle of a pattern), and
// Match4.feature [4] (an unbounded * over a 21-hop chain); and for the pattern
// predicate, expressions/pattern/Pattern1.feature [7]-[9], [10], [16]-[18].
//
// Each consumer route is asserted against the MATCH baseline on the same
// pattern and graph, and the baseline itself against absolute answers on the
// shapes that decide the defect, so the routes cannot agree on a wrong answer.
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
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pvFixture is one graph over (:N {k:<name>}) nodes. Every node named by an
// edge is created, as are a, b and c.
type pvFixture struct {
	name  string
	edges []string // "a R b" = (a)-[:R]->(b)
}

// pvChain returns the edges of a directed :R chain c00 → c01 → … → c<n>.
func pvChain(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("c%02d R c%02d", i, i+1))
	}
	return out
}

var pvFixtures = []pvFixture{
	{"single", []string{"a R b"}},
	{"two_cycle", []string{"a R b", "b R a"}},
	{"parallel", []string{"a R b", "a R b"}},
	{"self_loop", []string{"a R a"}},
	{"self_loop_and_edge", []string{"a R a", "a R b"}},
	{"chain", []string{"a R b", "b R c"}},
	// The shortcut a → c hides the length-2 path a → b → c from a BFS whose
	// visited set records c at depth 1 (rmp #2898).
	{"triangle_shortcut", []string{"a R b", "b R c", "a R c"}},
	{"directed_triangle", []string{"a R b", "b R c", "c R a"}},
	{"parallel_then_edge", []string{"a R b", "a R b", "b R c"}},
	{"mixed_types", []string{"a R b", "b S c", "a S c"}},
	// 17 hops: above the evaluator's 15-hop default for an unbounded range,
	// below MATCH's (Match4 [4] runs 21 hops).
	{"long_chain", pvChain(17)},
}

func pvEngine(t *testing.T, fx pvFixture) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	names := []string{"a", "b", "c"}
	for _, e := range fx.edges {
		f := strings.Fields(e)
		for _, n := range []string{f[0], f[2]} {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	nodes := make([]string, len(names))
	for i, n := range names {
		nodes[i] = fmt.Sprintf(`(:N {k:'%s'})`, n)
	}
	qs := make([]string, 0, 1+len(fx.edges))
	qs = append(qs, `CREATE `+strings.Join(nodes, ", "))
	for _, e := range fx.edges {
		f := strings.Fields(e)
		qs = append(qs, fmt.Sprintf(`MATCH (x:N {k:'%s'}), (y:N {k:'%s'}) CREATE (x)-[:%s]->(y)`, f[0], f[2], f[1]))
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

// pvRows runs query and returns its rows rendered as "c1|c2|…" over cols,
// sorted. An error is returned as the single row "ERROR: …" so that a route
// that fails to run is reported as a mismatch rather than aborting the matrix.
func pvRows(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
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

// pvPatterns are the probed patterns. a and b are the only named variables, as
// a pattern predicate may not introduce new ones (Pattern1 [10], "Fail on
// introducing unbounded variables in pattern"); twoEnds says whether b occurs.
var pvPatterns = []struct {
	name    string
	pattern string
	twoEnds bool
}{
	// Range forms, directed.
	{"exact2", `(a)-[:R*2]->(b)`, true},
	{"range1_3", `(a)-[:R*1..3]->(b)`, true},
	{"range0_2", `(a)-[:R*0..2]->(b)`, true},
	{"zero", `(a)-[:R*0]->(b)`, true},
	{"unbounded_dots", `(a)-[:R*..]->(b)`, true},
	{"unbounded_star", `(a)-[:R*]->(b)`, true},
	{"exact2_anon", `(a)-[:R*2]->()`, false},
	// Direction.
	{"exact2_undirected", `(a)-[:R*2]-(b)`, true},
	{"range1_2_incoming", `(a)<-[:R*1..2]-(b)`, true},
	{"exact2_undirected_anon", `(a)-[:R*2]-()`, false},
	{"exact2_untyped", `(a)-[*2]->(b)`, true},
	// Cycles back to the start node.
	{"cycle_exact2", `(a)-[:R*2]->(a)`, false},
	{"cycle_exact1", `(a)-[:R*1]->(a)`, false},
	{"cycle_directed_range", `(a)-[:R*1..3]->(a)`, false},
	{"cycle_incoming_unbounded", `(a)<-[:R*]-(a)`, false},
	{"cycle_directed_exact1_max1", `(a)-[:R*..1]->(a)`, false},
	{"cycle_undirected_range", `(a)-[:R*1..3]-(a)`, false},
	{"cycle_undirected_exact2", `(a)-[:R*2]-(a)`, false},
	// Mixed fixed and variable-length hops: the variable-length hop must not
	// re-use a relationship a fixed hop crossed, and vice versa.
	{"fixed_then_var", `(a)-[:R]->(b)<-[:R*1..1]-(a)`, true},
	{"var_then_fixed", `(a)-[:R*1..1]->(b)<-[:R]-(a)`, true},
	{"var_then_var", `(a)-[:R*1]->(b)<-[:R*1]-(a)`, true},
	{"fixed_then_var_undirected", `(a)-[:R]-(b)-[:R*1..2]-(a)`, true},
	{"zero_then_fixed", `(a)-[:R*0..1]->()-[:R]->(b)`, true},
	{"var_then_fixed_type", `(a)-[:R*1..2]->()-[:S]->(b)`, true},
}

// TestPatternPredicate_VarLength_MatchesMatch asserts, for every fixture ×
// pattern, that each consumer route yields exactly the bag MATCH yields.
func TestPatternPredicate_VarLength_MatchesMatch(t *testing.T) {
	t.Parallel()
	for _, fx := range pvFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := pvEngine(t, fx)
			for _, p := range pvPatterns {
				t.Run(p.name, func(t *testing.T) {
					outer, cols, keys, withVars := `MATCH (a:N)`, []string{"ka"}, `a.k AS ka`, `a`
					if p.twoEnds {
						outer, cols, keys, withVars = `MATCH (a:N), (b:N)`, []string{"ka", "kb"}, `a.k AS ka, b.k AS kb`, `a, b`
					}
					distinct := pvRows(t, eng, `MATCH `+p.pattern+` WHERE a:N RETURN DISTINCT `+keys, cols...)
					counted := pvRows(t, eng, `MATCH `+p.pattern+` WHERE a:N RETURN `+keys+`, count(*) AS n`, append(cols, "n")...)
					routes := []struct {
						name  string
						query string
						cols  []string
						want  []string
					}{
						{"where_predicate", outer + ` WHERE ` + p.pattern + ` RETURN ` + keys, cols, distinct},
						{"where_not_predicate", outer + ` WHERE NOT ` + p.pattern + ` RETURN ` + keys, cols, nil},
						{"exists_subquery", outer + ` WHERE EXISTS { ` + p.pattern + ` } RETURN ` + keys, cols, distinct},
						{"comprehension_return", outer + ` RETURN ` + keys + `, size([` + p.pattern + ` | 1]) AS n`,
							append(cols, "n"), counted},
						{"comprehension_with_where", outer + ` WITH ` + withVars + `, size([` + p.pattern + ` | 1]) AS n ` +
							`WHERE n > 0 RETURN ` + keys, cols, distinct},
						{"comprehension_unwind", outer + ` UNWIND [` + p.pattern + ` | 1] AS x RETURN ` + keys + `, count(x) AS n`,
							append(cols, "n"), counted},
					}
					all := pvRows(t, eng, outer+` RETURN `+keys, cols...)
					for _, r := range routes {
						want := r.want
						if r.name == "where_not_predicate" {
							// NOT p holds exactly on the outer rows p rejects.
							want = slices.DeleteFunc(slices.Clone(all), func(s string) bool { return slices.Contains(distinct, s) })
						}
						got := pvRows(t, eng, r.query, r.cols...)
						if r.name == "comprehension_return" {
							got = slices.DeleteFunc(got, func(s string) bool { return strings.HasSuffix(s, "|0") })
						}
						if strings.Join(got, ";") != strings.Join(want, ";") {
							t.Errorf("MISMATCH route=%s fixture=%s pattern=%s\n  query %q\n  got  %v\n  want %v (MATCH baseline)",
								r.name, fx.name, p.name, r.query, got, want)
						}
					}
				})
			}
		})
	}
}

// TestPatternPredicate_VarLength_MatchBaselineAbsolute pins the MATCH baseline
// the parity test compares against, on the shapes that decide rmp #2898.
func TestPatternPredicate_VarLength_MatchBaselineAbsolute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		query   string
		want    []string
	}{
		// a → b → c is a length-2 path although a → c is also an edge.
		{"triangle_shortcut", `MATCH (a:N)-[:R*2]->(b:N) RETURN a.k + b.k AS k`, []string{`"ac"`}},
		{"triangle_shortcut", `MATCH (a:N)-[:R*1..3]->(b:N) RETURN a.k + b.k AS k`, []string{`"ab"`, `"ac"`, `"ac"`, `"bc"`}},
		// One relationship cannot fill a fixed and a variable-length slot.
		{"single", `MATCH (a:N)-[:R]->(b:N)<-[:R*1..1]-(a) RETURN a.k AS k`, nil},
		{"parallel", `MATCH (a:N)-[:R]->(b:N)<-[:R*1..1]-(a) RETURN a.k AS k`, []string{`"a"`, `"a"`}},
		// The same, with the variable-length slot first: a fixed hop AFTER a
		// variable-length one must not re-use a relationship it crossed.
		{"single", `MATCH (a:N)-[:R*1..1]->(b:N)<-[:R]-(a) RETURN a.k AS k`, nil},
		{"self_loop_and_edge", `MATCH (a:N)-[:R*1..1]->(b:N)<-[:R]-(a) RETURN a.k + b.k AS k`, nil},
		{"parallel", `MATCH (a:N)-[:R*1..1]->(b:N)<-[:R]-(a) RETURN a.k AS k`, []string{`"a"`, `"a"`}},
		{"single", `MATCH (a:N)-[:R*1]->(b:N)<-[:R*1]-(a) RETURN a.k AS k`, nil},
		// A 2-cycle returns to its start in two hops, from either node.
		{"two_cycle", `MATCH (a:N)-[:R*2]->(a) RETURN a.k AS k`, []string{`"a"`, `"b"`}},
		// A self-loop is a length-1 cycle, and cannot be crossed twice.
		{"self_loop", `MATCH (a:N)-[:R*1]->(a) RETURN a.k AS k`, []string{`"a"`}},
		{"self_loop", `MATCH (a:N)-[:R*2]-(a) RETURN a.k AS k`, nil},
		// *0 binds both ends to the start node.
		{"single", `MATCH (a:N)-[:R*0]->(b:N) RETURN a.k + b.k AS k`, []string{`"aa"`, `"bb"`, `"cc"`}},
		// An unbounded range reaches the end of a 17-hop chain.
		{"long_chain", `MATCH (a:N {k:'c00'})-[:R*]->(b:N {k:'c17'}) RETURN b.k AS k`, []string{`"c17"`}},
	} {
		t.Run(tc.fixture+"/"+tc.query, func(t *testing.T) {
			t.Parallel()
			var fx pvFixture
			for _, f := range pvFixtures {
				if f.name == tc.fixture {
					fx = f
				}
			}
			got := pvRows(t, pvEngine(t, fx), tc.query, "k")
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPatternPredicate_VarLength_BoundRelList pins a variable-length hop whose
// relationship variable is already bound to a list of relationships: the list
// stands for the path it spells, in order and direction
// (clauses/match/Match4.feature [8], Match9.feature [6] and [7]). Every route
// is asserted against an absolute answer, which MATCH is asserted against too.
func TestPatternPredicate_VarLength_BoundRelList(t *testing.T) {
	t.Parallel()
	fx := pvFixture{"chain_and_shortcut", []string{"a R b", "b R c", "a R c"}}
	eng := pvEngine(t, fx)
	// rs = the two relationships of a → b → c, in path order. The start node is
	// anonymous on purpose: naming it x would collide with the routes'
	// `UNWIND … AS x`, and an UNWIND alias that re-uses a name bound by a clause
	// before an intervening WITH loses every row — a planner defect independent of
	// this test's subject.
	const bind = `MATCH (:N {k:'a'})-[r1:R]->(:N {k:'b'})-[r2:R]->(:N {k:'c'}) WITH [r1, r2] AS rs `
	for _, tc := range []struct {
		name    string
		pattern string
		want    []string
	}{
		{"forward", `(a)-[rs*]->(b)`, []string{`"a"|"c"`}},
		{"forward_range_ok", `(a)-[rs*2..2]->(b)`, []string{`"a"|"c"`}},
		{"forward_range_too_short", `(a)-[rs*1..1]->(b)`, nil},
		{"wrong_direction", `(a)<-[rs*]-(b)`, nil},
		{"undirected", `(a)-[rs*]-(b)`, []string{`"a"|"c"`}},
		// The fixed hop may not re-use a relationship of the bound list.
		{"then_fixed_reuse", `(a)-[rs*]->(b)<-[:R]-()`, []string{`"a"|"c"`}},
		{"then_fixed_reverse_reuse", `(a)-[rs*]->(b)<-[:R]-(:N {k:'b'})`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols := []string{"ka", "kb"}
			outer := bind + `MATCH (a:N), (b:N) `
			for _, r := range []struct{ name, query string }{
				{"match", bind + `MATCH ` + tc.pattern + ` RETURN DISTINCT a.k AS ka, b.k AS kb`},
				{"where_predicate", outer + `WHERE ` + tc.pattern + ` RETURN a.k AS ka, b.k AS kb`},
				{"exists_subquery", outer + `WHERE EXISTS { ` + tc.pattern + ` } RETURN a.k AS ka, b.k AS kb`},
				{"comprehension_with_where", outer + `WITH a, b, size([` + tc.pattern + ` | 1]) AS n WHERE n > 0 RETURN a.k AS ka, b.k AS kb`},
				{"comprehension_unwind", outer + `UNWIND [` + tc.pattern + ` | 1] AS x RETURN DISTINCT a.k AS ka, b.k AS kb`},
			} {
				got := pvRows(t, eng, r.query, cols...)
				if strings.Join(got, ";") != strings.Join(tc.want, ";") {
					t.Errorf("%s: %q\n  got  %v\n  want %v", r.name, r.query, got, tc.want)
				}
			}
		})
	}
}

// TestPatternComprehension_VarLength_BindsRelationshipList asserts that the
// evaluator's comprehension route binds a variable-length hop's relationship
// variable to the relationships it crossed, in traversal order, exactly as MATCH
// does (Match9.feature [1]).
func TestPatternComprehension_VarLength_BindsRelationshipList(t *testing.T) {
	t.Parallel()
	const proj = `[x IN r | startNode(x).k + '>' + endNode(x).k]`
	for _, fx := range pvFixtures[:10] {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := pvEngine(t, fx)
			for _, pattern := range []string{`(a)-[r:R*1..2]->(b)`, `(a)-[r:R*0..2]-(b)`, `(a)<-[r:R*2]-(b)`} {
				want := pvRows(t, eng, `MATCH (a:N {k:'a'}) MATCH `+pattern+` RETURN b.k AS kb, `+proj+` AS rs`, "kb", "rs")
				// UNWIND is not hoisted: the expression-level evaluator runs it.
				got := pvRows(t, eng, `MATCH (a:N {k:'a'}) UNWIND [`+pattern+` | [b.k, `+proj+`]] AS x RETURN x[0] AS kb, x[1] AS rs`, "kb", "rs")
				if strings.Join(got, ";") != strings.Join(want, ";") {
					t.Errorf("%s:\n  got  %v\n  want %v (MATCH baseline)", pattern, got, want)
				}
			}
		})
	}
}

// TestPatternPredicate_VarLength_TraversalBudget asserts that the exact search is
// bounded by the traversal budget MATCH applies, and fails with the same error
// rather than running unbounded: the relationship-isomorphic paths of a complete
// directed graph on eight nodes are far more than the per-row limit, and a
// :Missing end node makes every one of them fail.
func TestPatternPredicate_VarLength_TraversalBudget(t *testing.T) {
	t.Parallel()
	var edges []string
	for i := range 8 {
		for j := range 8 {
			if i != j {
				edges = append(edges, fmt.Sprintf("k%d R k%d", i, j))
			}
		}
	}
	eng := pvEngine(t, pvFixture{"complete8", edges})
	got := pvRows(t, eng, `MATCH (a:N {k:'k0'}) WHERE (a)-[:R*2..]->(:Missing) RETURN a.k AS k`, "k")
	if len(got) != 1 || !strings.Contains(got[0], "variable-length expand safety cap exceeded") {
		t.Fatalf("got %v, want the variable-length safety-cap error", got)
	}
}
