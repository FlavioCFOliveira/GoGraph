package cypher_test

// pattern_eval_rel_uniqueness_test.go — rmp #2895.
//
// openCypher matches a pattern under relationship isomorphism: one graph
// relationship may fill at most one relationship slot of a single pattern. The
// TCK pins it for MATCH (clauses/match/Match3.feature [29] rejects a re-used
// relationship variable with RelationshipUniquenessViolation) and relies on it
// inside a pattern predicate (expressions/pattern/Pattern1.feature [10] and
// [18]: `(n)-[:REL1*2]-()` excludes (:A), whose only length-2 REL1 walk crosses
// the same relationship twice).
//
// Every consumer of a pattern must therefore agree with MATCH on the same
// pattern: the WHERE pattern predicate, EXISTS { }, and a pattern
// comprehension both where the translator hoists it (RETURN) and where the
// expression-level evaluator runs it (UNWIND). Each case below is asserted
// against the MATCH baseline, and the baseline itself against an absolute
// answer, so the routes cannot agree on a wrong one.
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

// puFixture is one graph, given as the CREATE statements that build it
// over nodes (:N {k:'a'}), (:N {k:'b'}), (:N {k:'c'}).
type puFixture struct {
	name  string
	edges []string // "a R b" = (a)-[:R]->(b)
}

var puFixtures = []puFixture{
	{"single", []string{"a R b"}},
	{"two_cycle", []string{"a R b", "b R a"}},
	{"parallel", []string{"a R b", "a R b"}},
	{"self_loop", []string{"a R a"}},
	{"two_self_loops", []string{"a R a", "a R a"}},
	{"triangle", []string{"a R b", "b R c", "c R a"}},
	{"chain", []string{"a R b", "b R c"}},
	{"mixed_types", []string{"a R b", "b S a"}},
	{"parallel_mixed_types", []string{"a R b", "a S b"}},
}

func puEngine(t *testing.T, fx puFixture) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	qs := make([]string, 0, 1+len(fx.edges))
	qs = append(qs, `CREATE (:N {k:'a'}), (:N {k:'b'}), (:N {k:'c'})`)
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

// puRows runs query and returns its rows rendered as "c1|c2|…" over cols,
// sorted, so two routes that emit the same bag compare equal.
func puRows(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close(%q): %v", query, cerr)
		}
	}()
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf("%v", rec[c])
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	sort.Strings(out)
	return out
}

// puPatterns are the probed patterns. a and b are the only named
// variables, as a pattern predicate may not introduce new ones (Pattern1 [10],
// "Fail on introducing unbounded variables in pattern"). twoEnds says whether b
// occurs, and so whether the outer MATCH binds it.
//
// withParseFails marks the one pattern the WITH … WHERE route cannot run: the
// parser panics on `WITH a, b, size([(a)-[]-(b)-[]-(a) | 1]) AS n` (an untyped
// two-hop comprehension in a WITH projection), independently of this defect, so
// that route is left to the other shapes for it.
var puPatterns = []struct {
	name           string
	pattern        string
	twoEnds        bool
	withParseFails bool
}{
	{"one_hop_undirected", `(a)-[:R]-(b)`, true, false},
	{"one_hop_self", `(a)-[:R]-(a)`, false, false},
	{"one_hop_self_directed", `(a)-[:R]->(a)`, false, false},
	{"two_hop_undirected_cycle", `(a)-[:R]-(b)-[:R]-(a)`, true, false},
	{"two_hop_directed_cycle", `(a)-[:R]->(b)-[:R]->(a)`, true, false},
	{"two_hop_parallel_out_in", `(a)-[:R]->(b)<-[:R]-(a)`, true, false},
	{"two_hop_parallel_in_out", `(a)<-[:R]-(b)-[:R]->(a)`, true, false},
	{"two_hop_anon_cycle", `(a)-[:R]-()-[:R]-(a)`, false, false},
	{"two_hop_anon_open", `(a)-[:R]-()-[:R]-()`, false, false},
	{"two_hop_self_twice", `(a)-[:R]-(a)-[:R]-(a)`, false, false},
	{"two_hop_incoming_chain", `(a)<-[:R]-()<-[:R]-()`, false, false},
	{"two_hop_untyped_cycle", `(a)-[]-(b)-[]-(a)`, true, true},
	{"three_hop_anon_cycle", `(a)-[:R]-()-[:R]-()-[:R]-(a)`, false, false},
	{"three_hop_directed_cycle", `(a)-[:R]->()-[:R]->()-[:R]->(a)`, false, false},
	{"three_hop_bound", `(a)-[:R]-(b)-[:R]-()-[:R]-(a)`, true, false},
}

// TestPatternPredicate_RelationshipUniqueness_MatchesMatch asserts, for every
// fixture × pattern, that each consumer route yields exactly the (a, b) bag
// MATCH yields.
func TestPatternPredicate_RelationshipUniqueness_MatchesMatch(t *testing.T) {
	t.Parallel()
	for _, fx := range puFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := puEngine(t, fx)
			for _, p := range puPatterns {
				t.Run(p.name, func(t *testing.T) {
					outer, cols, keys, withVars := `MATCH (a:N)`, []string{"ka"}, `a.k AS ka`, `a`
					if p.twoEnds {
						outer, cols, keys, withVars = `MATCH (a:N), (b:N)`, []string{"ka", "kb"}, `a.k AS ka, b.k AS kb`, `a, b`
					}
					// MATCH yields one row per match; the existential routes one
					// row per (a, b) with at least one.
					distinct := puRows(t, eng, `MATCH `+p.pattern+` WHERE a:N RETURN DISTINCT `+keys, cols...)
					counted := puRows(t, eng, `MATCH `+p.pattern+` WHERE a:N RETURN `+keys+`, count(*) AS n`, append(cols, "n")...)
					routes := []struct {
						name  string
						query string
						cols  []string
						want  []string
					}{
						{"where_predicate", outer + ` WHERE ` + p.pattern + ` RETURN ` + keys, cols, distinct},
						{"exists_subquery", outer + ` WHERE EXISTS { ` + p.pattern + ` } RETURN ` + keys, cols, distinct},
						// RETURN hoists the comprehension into a RollUpApply; the
						// zero-size rows are dropped below so the bag compares with
						// MATCH's count(*).
						{"comprehension_return", outer + ` RETURN ` + keys + `, size([` + p.pattern + ` | 1]) AS n`,
							append(cols, "n"), counted},
						// A WITH … WHERE over the projected size re-evaluates the
						// comprehension in the filter.
						{"comprehension_with_where", outer + ` WITH ` + withVars + `, size([` + p.pattern + ` | 1]) AS n ` +
							`WHERE n > 0 RETURN ` + keys, cols, distinct},
						// UNWIND is not hoisted: the expression-level evaluator runs it.
						{"comprehension_unwind", outer + ` UNWIND [` + p.pattern + ` | 1] AS x RETURN ` + keys + `, count(x) AS n`,
							append(cols, "n"), counted},
					}
					for _, r := range routes {
						if p.withParseFails && r.name == "comprehension_with_where" {
							continue
						}
						got := puRows(t, eng, r.query, r.cols...)
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

// TestPatternPredicate_RelationshipUniqueness_MatchBaselineAbsolute pins the
// MATCH baseline the parity test compares against, on the shapes whose answer
// turns on uniqueness.
func TestPatternPredicate_RelationshipUniqueness_MatchBaselineAbsolute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		query   string
		want    []string
	}{
		// One relationship cannot fill both hops of the 2-cycle.
		{"single", `MATCH (a:N)-[:R]-(b:N)-[:R]-(a) RETURN a.k AS k`, nil},
		{"two_cycle", `MATCH (a:N)-[:R]-(b:N)-[:R]-(a) RETURN a.k AS k`, []string{`"a"`, `"a"`, `"b"`, `"b"`}},
		{"parallel", `MATCH (a:N)-[:R]->(b:N)<-[:R]-(a) RETURN a.k AS k`, []string{`"a"`, `"a"`}},
		{"self_loop", `MATCH (a:N)-[:R]-(a)-[:R]-(a) RETURN a.k AS k`, nil},
		{"two_self_loops", `MATCH (a:N)-[:R]-(a)-[:R]-(a) RETURN a.k AS k`, []string{`"a"`, `"a"`}},
		{"chain", `MATCH (a:N)-[:R]-()-[:R]-() RETURN a.k AS k`, []string{`"a"`, `"c"`}},
	} {
		t.Run(tc.fixture+"/"+tc.query, func(t *testing.T) {
			t.Parallel()
			var fx puFixture
			for _, f := range puFixtures {
				if f.name == tc.fixture {
					fx = f
				}
			}
			got := puRows(t, puEngine(t, fx), tc.query, "k")
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
