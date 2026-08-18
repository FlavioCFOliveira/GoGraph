package cypher_test

// pattern_comp_consumer_parity_test.go — rmp #2505.
//
// A pattern comprehension must produce the SAME bindings whichever clause
// consumes it. Two in-tree references already agree on what those bindings are:
//
//   - the MATCH baseline, `MATCH (c)<-[r:KNOWS]-(x) RETURN ...`; and
//   - the RETURN-projection route of the identical comprehension,
//     `RETURN [(c)<-[r:KNOWS]-(x) | ...]`, which the IR translator hoists into a
//     RollUpApply (cypher/ir/pattern_comprehension.go) and therefore evaluates
//     through the real Expand operator.
//
// A comprehension consumed by UNWIND is NOT hoisted — projectionsWithComprehensions
// is called only from translateReturn and translateWith — so it survives to
// evaluation as a raw *ast.PatternComprehension and is evaluated by the
// expression-level fallback, cypher.patternEvaluator.EvalPatternComp. Before
// #2505 that fallback diverged from both references in four ways:
//
//   - the traversal did not advance on a REVERSE leg (incoming, and the reverse
//     half of an undirected hop), because candidateHop records STORAGE order and
//     enumerateSteps read dstID — the anchor — instead of the other endpoint;
//   - id(r) was 0 for EVERY hop in BOTH directions, because relValueFromHop never
//     populated RelationshipValue.ID;
//   - startNode/endNode were transposed on a reverse leg, and the property lookup
//     used those transposed keys so properties came back null;
//   - a typed hop over a multi-type parallel pair emitted one row per parallel
//     edge rather than one per edge of the requested type, because the type
//     filter was per-PAIR (an existence test) rather than per-slot.
//
// These tests pin every route against the MATCH baseline over a directed
// multigraph, so the per-instance edge handle is live. The consumer is the
// discriminator, so each shape is asserted through MATCH, RETURN, UNWIND, WITH,
// and a list comprehension wrapping the pattern comprehension.
//
// Layer: short. goleak-clean (engine and graph are local).

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pcParityEngine builds the #2505 fixture: a directed multigraph holding the
// chain A -> B -> C over :KNOWS, each edge carrying a distinguishing `tag`, plus
// a PARALLEL A -> B edge of a different type (:LIKES) so per-instance identity,
// per-instance properties and the per-slot type filter are all observable. The
// chain means C has exactly one incoming :KNOWS (from B) and A exactly one
// outgoing :KNOWS (to B), so each single-hop probe has one correct answer.
func pcParityEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:Person {name:'A'})`,
		`CREATE (:Person {name:'B'})`,
		`CREATE (:Person {name:'C'})`,
		`MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS {tag:'AB'}]->(b)`,
		`MATCH (b:Person {name:'B'}),(c:Person {name:'C'}) CREATE (b)-[:KNOWS {tag:'BC'}]->(c)`,
		`MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:LIKES {tag:'AB2'}]->(b)`,
	} {
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

// pcScalars runs query and returns column col of every row, rendered with %v and
// in result order. Every query below is deterministic in both row count and row
// order, so no sort is applied — a change in cardinality or in order is itself a
// divergence this test must catch.
func pcScalars(t *testing.T, eng *cypher.Engine, query, col string) []string {
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
		out = append(out, fmt.Sprintf("%v", res.Record()[col]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return out
}

// pcEqual compares two rendered row slices element for element.
func pcEqual(a, b []string) bool { return slices.Equal(a, b) }

// ─────────────────────────────────────────────────────────────────────────────
// The reference: absolute answers, so a regression in the BASELINE is visible
// rather than silently re-baselining every parity assertion below.
// ─────────────────────────────────────────────────────────────────────────────

// TestPatternCompParity_MatchBaseline_Absolute pins the MATCH baseline for the
// incoming and outgoing single hops, and for the undirected hop. Without this,
// the parity tests could all agree on a wrong answer.
func TestPatternCompParity_MatchBaseline_Absolute(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"incoming", `MATCH (c:Person {name:'C'})<-[r:KNOWS]-(x:Person) ` +
			`RETURN [x.name, r.tag, startNode(r).name, endNode(r).name] AS t`,
			[]string{`["B", "BC", "B", "C"]`}},
		{"outgoing", `MATCH (a:Person {name:'A'})-[r:KNOWS]->(x:Person) ` +
			`RETURN [x.name, r.tag, startNode(r).name, endNode(r).name] AS t`,
			[]string{`["B", "AB", "A", "B"]`}},
		// The undirected hop from B reaches C forwards and A backwards. The
		// reverse leg must still report the STORED orientation A→B (#2504), and
		// the :KNOWS filter must exclude the parallel :LIKES slot.
		{"undirected", `MATCH (b:Person {name:'B'})-[r:KNOWS]-(o:Person) ` +
			`RETURN [o.name, r.tag, startNode(r).name, endNode(r).name] AS t`,
			[]string{`["C", "BC", "B", "C"]`, `["A", "AB", "A", "B"]`}},
		// The untyped hop over the parallel pair must report each instance's OWN
		// type and OWN properties, not the pair's coalesced union.
		{"parallel_untyped", `MATCH (a:Person {name:'A'})-[r]->(b:Person {name:'B'}) ` +
			`RETURN [type(r), r.tag] AS t`,
			[]string{`["KNOWS", "AB"]`, `["LIKES", "AB2"]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pcScalars(t, eng, tc.query, "t")
			if !pcEqual(got, tc.want) {
				t.Fatalf("MATCH baseline %s: got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Route parity: the consumer must not change the bindings.
// ─────────────────────────────────────────────────────────────────────────────

// pcConsumers builds, for one comprehension body and its anchor MATCH, the four
// consumer routes plus the MATCH baseline that is the reference. body is the
// comprehension WITHOUT its surrounding brackets, e.g.
// `(c)<-[r:KNOWS]-(x:Person) | x.name`.
//
// The RETURN and WITH routes project the whole list and the assertions compare
// against the baseline's rows, so a cardinality divergence shows up as a
// different list length rather than being masked by taking element [0].
func pcConsumers(anchor, body string) map[string]string {
	comp := "[" + body + "]"
	return map[string]string{
		"RETURN":   anchor + ` RETURN ` + comp + ` AS l`,
		"WITH":     anchor + ` WITH ` + comp + ` AS l RETURN l AS l`,
		"listcomp": anchor + ` RETURN [y IN ` + comp + ` | y] AS l`,
		"UNWIND":   anchor + ` UNWIND ` + comp + ` AS l RETURN collect(l) AS l`,
	}
}

// assertAllRoutesMatchBaseline is the core oracle: it renders the MATCH baseline
// as a list and requires every consumer route of the equivalent comprehension to
// produce exactly that list — same values, same count, same order.
func assertAllRoutesMatchBaseline(t *testing.T, eng *cypher.Engine, baseline, anchor, body string) {
	t.Helper()
	want := pcScalars(t, eng, baseline, "l")
	if len(want) != 1 {
		t.Fatalf("baseline %q must project exactly one list row, got %v", baseline, want)
	}
	if want[0] == "[]" {
		t.Fatalf("baseline %q collected an EMPTY list — the fixture cannot discriminate", baseline)
	}
	for route, query := range pcConsumers(anchor, body) {
		t.Run(route, func(t *testing.T) {
			got := pcScalars(t, eng, query, "l")
			if !pcEqual(got, want) {
				t.Fatalf("%s route: got %v, want %v (the MATCH baseline)", route, got, want)
			}
		})
	}
}

// TestPatternCompParity_Incoming is the core #2505 assertion: on an incoming hop
// the neighbour advances (B, not the anchor C), id(r) is the real handle, the
// properties read from the bound relationship, and startNode/endNode report the
// STORED orientation B→C rather than the transposed C→B.
func TestPatternCompParity_Incoming(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (c:Person {name:'C'})<-[r:KNOWS]-(x:Person) `+
			`RETURN collect([x.name, id(r), r.tag, type(r), startNode(r).name, endNode(r).name]) AS l`,
		`MATCH (c:Person {name:'C'})`,
		`(c)<-[r:KNOWS]-(x:Person) | [x.name, id(r), r.tag, type(r), startNode(r).name, endNode(r).name]`)
}

// TestPatternCompParity_Outgoing is the direction control. Outgoing bindings were
// already right on every route before #2505 EXCEPT id(r), which was 0 on the
// UNWIND route in both directions — so this arm is a real gate, not a formality.
func TestPatternCompParity_Outgoing(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (a:Person {name:'A'})-[r:KNOWS]->(x:Person) `+
			`RETURN collect([x.name, id(r), r.tag, type(r), startNode(r).name, endNode(r).name]) AS l`,
		`MATCH (a:Person {name:'A'})`,
		`(a)-[r:KNOWS]->(x:Person) | [x.name, id(r), r.tag, type(r), startNode(r).name, endNode(r).name]`)
}

// TestPatternCompParity_Undirected exercises both legs of one hop at once: the
// forward leg B→C and the reverse leg A→B. The reverse leg is where the anchor
// used to be re-bound, so an undirected comprehension used to report the anchor
// as its own neighbour.
func TestPatternCompParity_Undirected(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (b:Person {name:'B'})-[r:KNOWS]-(o:Person) `+
			`RETURN collect([o.name, id(r), r.tag, startNode(r).name, endNode(r).name]) AS l`,
		`MATCH (b:Person {name:'B'})`,
		`(b)-[r:KNOWS]-(o:Person) | [o.name, id(r), r.tag, startNode(r).name, endNode(r).name]`)
}

// TestPatternCompParity_ParallelInstances pins per-instance granularity over the
// multigraph pair: an untyped hop must report each parallel edge's OWN type,
// OWN id and OWN properties. The UNWIND route used to report the pair's
// coalesced property union, so both instances claimed the last writer's tag.
func TestPatternCompParity_ParallelInstances(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (a:Person {name:'A'})-[r]->(b:Person {name:'B'}) `+
			`RETURN collect([type(r), id(r), r.tag]) AS l`,
		`MATCH (a:Person {name:'A'})`,
		`(a)-[r]->(b:Person {name:'B'}) | [type(r), id(r), r.tag]`)
}

// TestPatternCompParity_TypedHopOverParallelPair is the cardinality gate. The
// pair A→B carries a :KNOWS and a :LIKES edge; a `[r:KNOWS]` hop must yield ONE
// row. The per-PAIR type filter used to admit both slots, so the comprehension
// over-enumerated where the MATCH baseline did not.
func TestPatternCompParity_TypedHopOverParallelPair(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (a:Person {name:'A'})-[r:KNOWS]->(b:Person {name:'B'}) `+
			`RETURN collect([type(r), r.tag]) AS l`,
		`MATCH (a:Person {name:'A'})`,
		`(a)-[r:KNOWS]->(b:Person {name:'B'}) | [type(r), r.tag]`)
	// The mirror filter must select the OTHER instance, proving the gate
	// discriminates rather than merely truncating to the first slot.
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (a:Person {name:'A'})-[r:LIKES]->(b:Person {name:'B'}) `+
			`RETURN collect([type(r), r.tag]) AS l`,
		`MATCH (a:Person {name:'A'})`,
		`(a)-[r:LIKES]->(b:Person {name:'B'}) | [type(r), r.tag]`)
}

// TestPatternCompParity_MultiHop covers the recursive arm of enumerateSteps: a
// two-hop comprehension, forwards and backwards. The reverse form used to stand
// still on the anchor for both hops and so reported the anchor as the far end.
func TestPatternCompParity_MultiHop(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (a:Person {name:'A'})-[:KNOWS]->(m:Person)-[:KNOWS]->(z:Person) `+
			`RETURN collect([m.name, z.name]) AS l`,
		`MATCH (a:Person {name:'A'})`,
		`(a)-[:KNOWS]->(m:Person)-[:KNOWS]->(z:Person) | [m.name, z.name]`)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (c:Person {name:'C'})<-[:KNOWS]-(m:Person)<-[:KNOWS]-(z:Person) `+
			`RETURN collect([m.name, z.name]) AS l`,
		`MATCH (c:Person {name:'C'})`,
		`(c)<-[:KNOWS]-(m:Person)<-[:KNOWS]-(z:Person) | [m.name, z.name]`)
}

// TestPatternCompParity_InnerPredicate covers the comprehension's own WHERE. On
// the UNWIND route the predicate ran against the wrongly-bound anchor, so the
// filter that should keep the single match discarded it and the list came back
// empty.
func TestPatternCompParity_InnerPredicate(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (c:Person {name:'C'})<-[r:KNOWS]-(x:Person) WHERE x.name = 'B' `+
			`RETURN collect(x.name) AS l`,
		`MATCH (c:Person {name:'C'})`,
		`(c)<-[r:KNOWS]-(x:Person) WHERE x.name = 'B' | x.name`)
}

// TestPatternCompParity_SimpleGraph repeats the incoming and undirected parity
// on a NON-multigraph, where every adjacency slot carries the 0 handle. Both
// per-instance ladders (type resolution and the per-slot type filter) must fall
// back to the per-pair surfaces there, so this arm guards the fallback the
// multigraph fixture never exercises.
func TestPatternCompParity_SimpleGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:Person {name:'A'})`,
		`CREATE (:Person {name:'B'})`,
		`CREATE (:Person {name:'C'})`,
		`MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS {tag:'AB'}]->(b)`,
		`MATCH (b:Person {name:'B'}),(c:Person {name:'C'}) CREATE (b)-[:KNOWS {tag:'BC'}]->(c)`,
	} {
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
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (c:Person {name:'C'})<-[r:KNOWS]-(x:Person) `+
			`RETURN collect([x.name, r.tag, type(r), startNode(r).name, endNode(r).name]) AS l`,
		`MATCH (c:Person {name:'C'})`,
		`(c)<-[r:KNOWS]-(x:Person) | [x.name, r.tag, type(r), startNode(r).name, endNode(r).name]`)
	assertAllRoutesMatchBaseline(t, eng,
		`MATCH (b:Person {name:'B'})-[r:KNOWS]-(o:Person) `+
			`RETURN collect([o.name, r.tag, startNode(r).name, endNode(r).name]) AS l`,
		`MATCH (b:Person {name:'B'})`,
		`(b)-[r:KNOWS]-(o:Person) | [o.name, r.tag, startNode(r).name, endNode(r).name]`)
}

// TestPatternCompParity_SelfLoop covers the endpoint case the chain fixture
// cannot reach. A self-loop is a real INCOMING edge, so `(s)<-[r]-(x)` must
// match it — MATCH and the hoisted route both did, while the fallback's
// "skip self" guard dropped it, so an incoming comprehension over a
// self-looping node came back empty. The undirected arm is the counterweight:
// openCypher matches each relationship of an undirected pattern exactly once,
// so the loop must appear ONCE there, never twice.
func TestPatternCompParity_SelfLoop(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:Person {name:'S'})`,
		`MATCH (s:Person {name:'S'}) CREATE (s)-[:KNOWS {tag:'loop'}]->(s)`,
	} {
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
	for _, tc := range []struct{ name, baseline, body string }{
		{"incoming",
			`MATCH (s:Person {name:'S'})<-[r:KNOWS]-(x:Person) ` +
				`RETURN collect([x.name, id(r), r.tag, startNode(r).name, endNode(r).name]) AS l`,
			`(s)<-[r:KNOWS]-(x:Person) | [x.name, id(r), r.tag, startNode(r).name, endNode(r).name]`},
		{"outgoing",
			`MATCH (s:Person {name:'S'})-[r:KNOWS]->(x:Person) ` +
				`RETURN collect([x.name, id(r), r.tag, startNode(r).name, endNode(r).name]) AS l`,
			`(s)-[r:KNOWS]->(x:Person) | [x.name, id(r), r.tag, startNode(r).name, endNode(r).name]`},
		{"undirected",
			`MATCH (s:Person {name:'S'})-[r:KNOWS]-(x:Person) ` +
				`RETURN collect([x.name, id(r), r.tag, startNode(r).name, endNode(r).name]) AS l`,
			`(s)-[r:KNOWS]-(x:Person) | [x.name, id(r), r.tag, startNode(r).name, endNode(r).name]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The baseline must see the loop exactly once in every direction —
			// otherwise the parity arms below would agree on nothing.
			base := pcScalars(t, eng, tc.baseline, "l")
			if len(base) != 1 || base[0] != `[["S", 1, "loop", "S", "S"]]` {
				t.Fatalf("%s baseline: got %v, want one row [["+`["S", 1, "loop", "S", "S"]`+"]]", tc.name, base)
			}
			assertAllRoutesMatchBaseline(t, eng, tc.baseline, `MATCH (s:Person {name:'S'})`, tc.body)
		})
	}
	// The existential predicate is the sibling path with the same self-skip: it
	// used to answer false for an incoming self-loop while MATCH and EXISTS { }
	// both answered true.
	for _, tc := range []struct{ name, query string }{
		{"where_incoming", `MATCH (s:Person) WHERE (s)<-[:KNOWS]-(:Person) RETURN s.name AS t`},
		{"where_outgoing", `MATCH (s:Person) WHERE (s)-[:KNOWS]->(:Person) RETURN s.name AS t`},
		{"where_undirected", `MATCH (s:Person) WHERE (s)-[:KNOWS]-(:Person) RETURN s.name AS t`},
		{"exists_incoming", `MATCH (s:Person) WHERE EXISTS { MATCH (s)<-[:KNOWS]-(:Person) } RETURN s.name AS t`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pcScalars(t, eng, tc.query, "t")
			if !pcEqual(got, []string{`"S"`}) {
				t.Fatalf("%s: got %v, want [\"S\"]", tc.name, got)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The sibling pattern-bearing constructs.
// ─────────────────────────────────────────────────────────────────────────────

// TestPatternCompParity_ExistsCountUnaffected proves the #2505 divergence does
// not reach EXISTS { } / COUNT { }. The #2505 defect was direction-SPECIFIC — an
// incoming or reverse leg went wrong while the outgoing leg was right — so the
// discriminating property is that these two constructs answer an incoming hop
// exactly as they answer the mirror-image outgoing hop over the same edge.
//
// Both absolute answers are pinned as well, so the symmetry assertion cannot be
// satisfied by two equally-wrong answers.
func TestPatternCompParity_ExistsCountUnaffected(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		// B→C seen forwards from B, and the same edge seen backwards from C.
		{"exists_outgoing_present",
			`MATCH (b:Person {name:'B'}) RETURN EXISTS { MATCH (b)-[:KNOWS]->(:Person) } AS t`, "true"},
		{"exists_incoming_present",
			`MATCH (c:Person {name:'C'}) RETURN EXISTS { MATCH (c)<-[:KNOWS]-(:Person) } AS t`, "true"},
		{"exists_outgoing_absent",
			`MATCH (c:Person {name:'C'}) RETURN EXISTS { MATCH (c)-[:KNOWS]->(:Person) } AS t`, "false"},
		{"exists_incoming_absent",
			`MATCH (a:Person {name:'A'}) RETURN EXISTS { MATCH (a)<-[:KNOWS]-(:Person) } AS t`, "false"},
		{"count_outgoing_one",
			`MATCH (b:Person {name:'B'}) RETURN COUNT { MATCH (b)-[:KNOWS]->(:Person) } AS t`, "1"},
		{"count_incoming_one",
			`MATCH (c:Person {name:'C'}) RETURN COUNT { MATCH (c)<-[:KNOWS]-(:Person) } AS t`, "1"},
		{"count_outgoing_zero",
			`MATCH (c:Person {name:'C'}) RETURN COUNT { MATCH (c)-[:KNOWS]->(:Person) } AS t`, "0"},
		{"count_incoming_zero",
			`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)<-[:KNOWS]-(:Person) } AS t`, "0"},
		// A has two outgoing parallel edges of different types; the typed counts
		// must discriminate in both directions.
		{"count_outgoing_typed",
			`MATCH (a:Person {name:'A'}) RETURN COUNT { MATCH (a)-[:LIKES]->(:Person) } AS t`, "1"},
		{"count_incoming_typed",
			`MATCH (b:Person {name:'B'}) RETURN COUNT { MATCH (b)<-[:LIKES]-(:Person) } AS t`, "1"},
		{"count_incoming_untyped_parallel",
			`MATCH (b:Person {name:'B'}) RETURN COUNT { MATCH (b)<-[]-(:Person) } AS t`, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pcScalars(t, eng, tc.query, "t")
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("%s: got %v, want [%s]", tc.name, got, tc.want)
			}
		})
	}
}

// TestPatternCompParity_WherePatternPredicate covers the existential pattern
// predicate in WHERE. It reaches patternEvaluator.EvalPattern (matchIncoming /
// matchOutgoing), a sibling of the enumeration path #2505 fixed, so it is the
// arm that proves the fix did not have to change the existential path — and that
// the existential path still agrees with MATCH on which nodes have an incoming
// or outgoing :KNOWS.
func TestPatternCompParity_WherePatternPredicate(t *testing.T) {
	t.Parallel()
	eng := pcParityEngine(t)
	for _, tc := range []struct {
		name      string
		predicate string
		baseline  string
		want      []string
	}{
		{"incoming", `(n)<-[:KNOWS]-(:Person)`,
			`MATCH (n:Person) WHERE EXISTS { MATCH (n)<-[:KNOWS]-(:Person) } RETURN n.name AS t`,
			[]string{`"B"`, `"C"`}},
		{"outgoing", `(n)-[:KNOWS]->(:Person)`,
			`MATCH (n:Person) WHERE EXISTS { MATCH (n)-[:KNOWS]->(:Person) } RETURN n.name AS t`,
			[]string{`"A"`, `"B"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// These two queries drive a whole-label scan, whose node order is
			// deliberately unspecified (see the WalkNodeIDs contract), so the SET
			// is the assertion and the order is not.
			got := pcScalars(t, eng,
				`MATCH (n:Person) WHERE `+tc.predicate+` RETURN n.name AS t`, "t")
			base := pcScalars(t, eng, tc.baseline, "t")
			sort.Strings(got)
			sort.Strings(base)
			if !pcEqual(got, tc.want) {
				t.Fatalf("WHERE %s: got %v, want %v", tc.predicate, got, tc.want)
			}
			if !pcEqual(base, tc.want) {
				t.Fatalf("EXISTS baseline %s: got %v, want %v", tc.name, base, tc.want)
			}
		})
	}
}
