package cypher_test

// pattern_eval_same_hop_refs_test.go — rmp #2916.
//
// The enumerating pattern evaluator (the pattern comprehension that is not
// hoisted into the plan: under UNWIND, nested in a list comprehension, and in a
// statement that writes) resolved every property map of a hop before choosing
// the hop's candidate, against the row that bound only the earlier elements. A
// map that read the hop's OWN relationship or end node — `(a)-[r]->(b {v: r.w})`,
// `(a)-[r {w: b.v}]->(b)`, `(x {v: x.v})-->(a)`, `(a)-[rs*]->(b {v: size(rs)})` —
// therefore read NULL and matched nothing, where MATCH matches.
//
// Semantics. A property map constrains its element as the equivalent predicate
// on the element does (clauses/match/Match2.feature [5], Match1.feature [4]).
// The openCypher TCK has no scenario for a map that reads a variable of its own
// pattern, so MATCH is the parity oracle, and its answers are pinned absolutely
// below: a map sees the element it sits on, the hop's relationship and end node,
// and every earlier element; a variable bound only by a LATER hop reads as NULL
// (so `(a {v: coalesce(b.v, 1)})-->(b)` keeps every a with v = 1). A variable-
// length hop's relationship map sees the whole list and the end node, and holds
// for every relationship of the path. Nothing in the TCK or in MATCH rejects such
// a reference, so no route raises an error for it.
//
// Routes. A bare pattern predicate (WHERE / WHERE NOT) may not introduce a
// variable (expressions/pattern/Pattern1.feature [10]), so every pattern here is
// rejected there with UndefinedVariable — asserted, because it is what makes the
// existential evaluator ([patternEvaluator.matchPattern]) unreachable for these
// maps. EXISTS { } and the hoisted comprehension run planned operators and are
// asserted as parity controls; the three enumerating routes are the regression.
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
)

// shrFixture is one graph over :N nodes carrying k and an integer v, whose :R
// relationships carry an integer w.
type shrFixture struct {
	name   string
	create string
}

var shrFixtures = []shrFixture{
	// Parallel a → b relationships of different w, a two-cycle a ⇄ b, and a
	// shortcut a → c.
	{"mixed", `CREATE (a:N {k:'a', v:1}), (b:N {k:'b', v:2}), (c:N {k:'c', v:1}),
		(a)-[:R {w:2}]->(b), (a)-[:R {w:1}]->(b), (b)-[:R {w:2}]->(c), (b)-[:R {w:1}]->(a), (a)-[:R {w:1}]->(c)`},
	// Self-loops, one of which carries the node's own v.
	{"loops", `CREATE (a:N {k:'a', v:1}), (b:N {k:'b', v:2}), (c:N {k:'c', v:3}),
		(a)-[:R {w:1}]->(a), (b)-[:R {w:1}]->(b), (a)-[:R {w:2}]->(b), (b)-[:R {w:3}]->(c), (c)-[:R {w:1}]->(a)`},
	// A directed chain whose w counts the hops from a.
	{"chain", `CREATE (a:N {k:'a', v:0}), (b:N {k:'b', v:1}), (c:N {k:'c', v:2}), (d:N {k:'d', v:3}),
		(a)-[:R {w:1}]->(b), (b)-[:R {w:2}]->(c), (c)-[:R {w:3}]->(d), (a)-[:R {w:2}]->(c)`},
}

// shrCases are patterns anchored on the outer variable a. The "same_hop" cases
// failed on the enumerating routes before #2916; the rest are controls that
// already agreed (earlier elements, later elements, outer-only maps).
var shrCases = []orCase{
	// A map reading the same hop's relationship or end node.
	{"same_hop/node_reads_rel", `(a)-[r:R]->(b {v: r.w})`},
	{"same_hop/node_reads_rel_incoming", `(a)<-[r:R]-(b {v: r.w})`},
	{"same_hop/node_reads_rel_undirected", `(a)-[r:R]-(b {v: r.w})`},
	{"same_hop/node_reads_rel_function", `(a)-[r:R]->(b {v: startNode(r).v + 1})`},
	{"same_hop/node_reads_rel_list_comprehension", `(a)-[r:R]->(b {v: [x IN [r.w] | x][0]})`},
	{"same_hop/node_reads_rel_and_literal", `(a)-[r:R]->(b {v: r.w, k: 'b'})`},
	{"same_hop/node_reads_itself", `(a)-[r:R]->(b {v: b.v})`},
	{"same_hop/rel_reads_end_node", `(a)-[r:R {w: b.v}]->(b)`},
	{"same_hop/rel_reads_end_node_incoming", `(a)<-[r:R {w: b.v}]-(b)`},
	{"same_hop/rel_reads_end_node_function", `(a)-[r:R {w: toInteger(b.v)}]->(b)`},
	{"same_hop/rel_reads_itself", `(a)-[r:R {w: r.w}]->(b)`},
	{"same_hop/start_reads_itself", `(x {v: x.v})-[:R]->(a)`},
	{"same_hop/second_hop_node_reads_rel", `(a)-[:R]->(m)-[s:R]->(c {v: s.w})`},
	{"same_hop/chain_of_reads", `(a)-[r:R]->(b {v: r.w})-[s:R {w: r.w}]->(c {v: s.w - 1})`},
	{"same_hop/varlen_end_reads_list", `(a)-[rs:R*1..2]->(b {v: size(rs)})`},
	{"same_hop/varlen_zero_end_reads_list", `(a)-[rs:R*0..2]->(b {v: size(rs) + 1})`},
	{"same_hop/varlen_rel_reads_list", `(a)-[rs:R*1..2 {w: size(rs)}]->()`},
	{"same_hop/varlen_zero_rel_reads_list", `(a)-[rs:R*0..2 {w: size(rs)}]->(b)`},
	{"same_hop/varlen_rel_reads_end_node", `(a)-[rs:R*1..2 {w: b.v}]->(b)`},
	{"same_hop/varlen_rel_and_end_read_list", `(a)-[rs:R*1..2 {w: size(rs)}]->(b {v: size(rs)})`},
	{"same_hop/varlen_then_hop_reads_rel", `(a)-[:R*0..1]->(m)-[s:R]->(c {v: s.w})`},
	// Controls: a map reading an earlier element, a later one, or the outer row.
	{"earlier/node_reads_start", `(a)-[:R]->(b {v: a.v})`},
	{"earlier/node_reads_earlier_node", `(a)-[:R]->(b)-[:R]->(c {v: b.v})`},
	{"earlier/rel_reads_earlier_rel", `(a)-[r:R]->()-[s:R {w: r.w}]->()`},
	{"earlier/node_reads_earlier_rel", `(a)-[r:R]->(m)-[s:R]->({v: r.w})`},
	{"earlier/rel_reads_hop_start", `(a)-[r:R {w: a.v}]->(b)`},
	{"earlier/rel_reads_earlier_node", `(a)-[r:R]->(b)-[s:R {w: b.v}]->(c)`},
	{"earlier/rel_reads_varlen_list", `(a)-[rs:R*1..2]->()-[s:R {w: size(rs)}]->()`},
	{"later/start_reads_later_node", `(a {v: coalesce(b.v, 1)})-[:R]->(b)`},
	{"later/start_reads_later_rel", `(x {v: r.w})-[r:R]->(a)`},
	{"later/node_reads_later_node", `(a)-[:R]->(b {v: c.v})-[:R]->(c)`},
	{"later/rel_reads_later_rel", `(a)-[r:R {w: s.w}]->()-[s:R]->()`},
}

// shrEngine builds a multigraph engine holding fx.
func shrEngine(t *testing.T, fx shrFixture) *cypher.Engine {
	t.Helper()
	eng := rbEngine(t, rbFixture{name: "empty"})
	mustRunWrite(t, eng, `MATCH (n) DETACH DELETE n`)
	mustRunWrite(t, eng, fx.create)
	return eng
}

// TestPatternSameHopRefs_MatchesMatch asserts, for every fixture × case, that
// each route over the pattern yields exactly the bag MATCH yields (#2916).
func TestPatternSameHopRefs_MatchesMatch(t *testing.T) {
	t.Parallel()
	const outer = `MATCH (a:N)`
	for _, fx := range shrFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			eng := shrEngine(t, fx)
			for _, c := range shrCases {
				t.Run(c.name, func(t *testing.T) {
					distinct := rbRows(t, eng, outer+` MATCH `+c.pattern+` RETURN DISTINCT a.k AS ka`, "ka")
					counted := rbRows(t, eng, outer+` MATCH `+c.pattern+` RETURN a.k AS ka, count(*) AS n`, "ka", "n")
					for _, want := range [][]string{distinct, counted} {
						if len(want) == 1 && strings.HasPrefix(want[0], "ERROR") {
							t.Fatalf("MATCH baseline failed: %v", want)
						}
					}
					routes := []struct {
						name, query string
						cols        []string
						want        []string
					}{
						{"exists_subquery", outer + ` WHERE EXISTS { ` + c.pattern + ` } RETURN a.k AS ka`, []string{"ka"}, distinct},
						{"comprehension_with_where", outer + ` WITH a, size([` + c.pattern + ` | 1]) AS n WHERE n > 0 RETURN a.k AS ka, n`,
							[]string{"ka", "n"}, counted},
						{"comprehension_return", outer + ` RETURN a.k AS ka, size([` + c.pattern + ` | 1]) AS n`,
							[]string{"ka", "n"}, counted},
						// The enumerating routes: not hoisted into the plan.
						{"comprehension_unwind", outer + ` UNWIND [` + c.pattern + ` | 1] AS y RETURN a.k AS ka, count(y) AS n`,
							[]string{"ka", "n"}, counted},
						{"comprehension_nested", outer + ` UNWIND [z IN [1] | [` + c.pattern + ` | z]][0] AS y RETURN a.k AS ka, count(y) AS n`,
							[]string{"ka", "n"}, counted},
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

// TestPatternSameHopRefs_WriteRoute asserts the same parity on a statement that
// writes, whose pattern evaluator is built by a different scaffold (#2916).
func TestPatternSameHopRefs_WriteRoute(t *testing.T) {
	t.Parallel()
	for _, c := range shrCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := shrEngine(t, shrFixtures[0])
			want := rbRows(t, eng, `MATCH (a:N) MATCH `+c.pattern+` RETURN a.k AS ka, count(*) AS n`, "ka", "n")
			q := `MATCH (a:N) CREATE (:Tmp) WITH a UNWIND [` + c.pattern + ` | 1] AS y RETURN a.k AS ka, count(y) AS n`
			if got := rbTxRows(t, eng, q, "ka", "n"); strings.Join(got, ";") != strings.Join(want, ";") {
				t.Errorf("%q\n  got  %v\n  want %v (MATCH baseline)", q, got, want)
			}
		})
	}
}

// TestPatternSameHopRefs_BarePredicateRejected asserts that a bare pattern
// predicate over any of these patterns is rejected at compile time, because each
// introduces a variable (expressions/pattern/Pattern1.feature [10]).
func TestPatternSameHopRefs_BarePredicateRejected(t *testing.T) {
	t.Parallel()
	eng := shrEngine(t, shrFixtures[0])
	for _, c := range shrCases {
		for _, q := range []string{
			`MATCH (a:N) WHERE ` + c.pattern + ` RETURN a.k AS ka`,
			`MATCH (a:N) WHERE NOT ` + c.pattern + ` RETURN a.k AS ka`,
		} {
			got := rbRows(t, eng, q, "ka")
			if len(got) != 1 || !strings.Contains(got[0], "UndefinedVariable") {
				t.Errorf("%s: %q: got %v, want an UndefinedVariable error", c.name, q, got)
			}
		}
	}
}

// TestPatternSameHopRefs_MatchBaselineAbsolute pins the MATCH baseline on the
// mixed fixture: a → b (w 2), a → b (w 1), b → c (w 2), b → a (w 1), a → c (w 1);
// v is 1, 2, 1 on a, b, c.
func TestPatternSameHopRefs_MatchBaselineAbsolute(t *testing.T) {
	t.Parallel()
	eng := shrEngine(t, shrFixtures[0])
	for _, tc := range []struct {
		query string
		want  []string
	}{
		// a → b (w 2 = b.v) and a → c (w 1 = c.v).
		{`MATCH (a:N {k:'a'})-[r:R]->(b {v: r.w}) RETURN b.k AS k`, []string{`"b"`, `"c"`}},
		{`MATCH (a:N {k:'a'})-[r:R {w: b.v}]->(b) RETURN b.k AS k`, []string{`"b"`, `"c"`}},
		// A self-reference constrains nothing beyond the property existing.
		{`MATCH (a:N {k:'a'})-[r:R {w: r.w}]->(b) RETURN b.k AS k`, []string{`"b"`, `"b"`, `"c"`}},
		// Every relationship of the path has w = size(rs): a -1-> b, a -1-> c,
		// and a -2-> b -2-> c.
		{`MATCH (a:N {k:'a'})-[rs:R*1..2 {w: size(rs)}]->(b) RETURN b.k AS k`, []string{`"b"`, `"c"`, `"c"`}},
		// A later variable reads as NULL: coalesce keeps all three of a's hops.
		{`MATCH (a:N {k:'a', v: 1}) MATCH (a {v: coalesce(b.v, 1)})-[:R]->(b) RETURN b.k AS k`, []string{`"b"`, `"b"`, `"c"`}},
	} {
		if got := rbRows(t, eng, tc.query, "k"); strings.Join(got, ";") != strings.Join(tc.want, ";") {
			t.Errorf("%q: got %v, want %v", tc.query, got, tc.want)
		}
	}
}

// rbTxRows is [rbRows] for a statement that writes, run through RunInTx.
func rbTxRows(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, rbParams)
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
