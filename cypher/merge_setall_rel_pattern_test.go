package cypher_test

// merge_setall_rel_pattern_test.go — regression coverage for a whole-entity
// ON CREATE / ON MATCH SET targeting the RELATIONSHIP variable of a MERGE
// pattern that routes to the MergePattern operator (rmp #2510).
//
// `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r += {w:'V'}`
// silently dropped the relationship write: the statement reported success, the
// relationship was created, and r.w read back null with no +properties counted.
// Two independent routing facts combined to lose it:
//
//   - the narrow MergeRelationship fast path (which owns the whole-entity
//     relationship KVAction machinery) requires BOTH endpoints already bound by
//     the child plan, so any fresh endpoint falls through to MergePattern; and
//     for both-bound endpoints extractRelKVActions additionally requires an
//     all-literal MapLiteral, so a parameter map falls through too;
//   - MergePattern.applySetAllActions resolved the action's target through
//     nodeIndexByVar only and skipped anything else, leaving a relationship
//     target to "the relationship machinery" — which is not the operator
//     running.
//
// The per-property form (`ON CREATE SET r.w = 'V'`) was never affected: it
// travels the mergeAction path, which has always dispatched a relationship
// target through hopIndexByRelVar. That asymmetry is what made the loss silent.
//
// The matrix below crosses the two operators (`+=`, `=`), the three right-hand
// side shapes (parameter map, all-literal map, literal map carrying a parameter
// value), both endpoint bindings (unbound, pre-bound) and both branches (ON
// CREATE, ON MATCH), for a relationship target and for a node target.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// runCountedP executes a write statement with parameters and returns its
// counters, so a lost write is caught as a missing side effect and not only as
// a missing value.
func runCountedP(t *testing.T, eng *cypher.Engine, query string, params map[string]any) *exec.QueryCounters {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, params)
	if err != nil {
		t.Fatalf("RunInTxAny(%q): %v", query, err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	c := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	return c
}

// setAllRHS is one right-hand-side shape for a whole-entity SET, with the
// parameters it needs. All three denote the same map, {w:'V'}.
type setAllRHS struct {
	params map[string]any
	name   string
	text   string
}

// setAllRHSShapes are the three RHS shapes a whole-entity SET accepts. Only the
// all-literal one satisfied extractRelKVActions, which is why the other two lost
// their write even with both endpoints bound.
var setAllRHSShapes = []setAllRHS{
	{name: "param_map", text: `$m`, params: map[string]any{"m": map[string]any{"w": "V"}}},
	{name: "literal_map", text: `{w:'V'}`, params: nil},
	{name: "literal_map_param_value", text: `{w:$w}`, params: map[string]any{"w": "V"}},
}

// setAllShape is one endpoint-binding × branch combination: the setup that puts
// the graph in the state the branch needs, the statement template (with %s for
// the SET operator and the RHS), and the side effects the statement must report.
// want.propsSet counts the endpoint name properties the statement itself writes
// plus the one property the whole-entity SET must write.
type setAllShape struct {
	name  string
	setup string
	stmt  string
	want  wantCounters
}

// relSetAllShapes crosses endpoint binding (unbound / pre-bound) with branch
// (ON CREATE / ON MATCH). The relationship pattern carries no inline property,
// so `=` and `+=` reach the same final state and share one expectation.
var relSetAllShapes = []setAllShape{
	{
		name:  "unbound_oncreate",
		setup: "",
		stmt:  `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r %s %s`,
		want: wantCounters{ // two endpoint names + w, plus the two :P labels
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:  "unbound_onmatch",
		setup: `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		stmt:  `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON MATCH SET r %s %s`,
		want:  wantCounters{propsSet: 1, containsUpdates: true}, // w only
	},
	{
		name:  "prebound_oncreate",
		setup: `CREATE (:P {name:'a'}),(:P {name:'b'})`,
		stmt:  `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON CREATE SET r %s %s`,
		want:  wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true}, // w only
	},
	{
		name:  "prebound_onmatch",
		setup: `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		stmt:  `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON MATCH SET r %s %s`,
		want:  wantCounters{propsSet: 1, containsUpdates: true}, // w only
	},
}

// setAllOperators are the two whole-entity SET operators. Over a
// property-free relationship they must reach the same state; `=` additionally
// tears down pre-existing properties, pinned separately by
// TestMergeSetAllRel_Replace_ClearsInlineProps.
var setAllOperators = []string{"+=", "="}

// TestMergeSetAllRel_Matrix is the whole-entity relationship-target matrix: every
// operator × RHS shape × endpoint binding × branch must write the property it
// names and count it. Before rmp #2510 every cell whose statement reached
// MergePattern — that is, every unbound cell, and every pre-bound cell whose RHS
// was not an all-literal map — reported success while writing nothing.
func TestMergeSetAllRel_Matrix(t *testing.T) {
	t.Parallel()
	for _, shape := range relSetAllShapes {
		for _, op := range setAllOperators {
			for _, rhs := range setAllRHSShapes {
				name := shape.name + "/" + opName(op) + "/" + rhs.name
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					eng := setMapEng(t)
					if shape.setup != "" {
						drainRunInTx(t, eng, shape.setup)
					}
					stmt := fmtStmt(shape.stmt, op, rhs.text)
					c := runCountedP(t, eng, stmt, rhs.params)
					assertCounters(t, stmt, c, shape.want)
					if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
						t.Fatalf("r.w = %q, want \"V\" after %s", got, stmt)
					}
				})
			}
		}
	}
}

// nodeSetAllShapes mirrors relSetAllShapes for a NODE target inside the same
// MergePattern-routed statements, answering whether the same deferral loses a
// whole-entity node action. The target is the pattern's first endpoint, so the
// written property lands on `a`.
var nodeSetAllShapes = []setAllShape{
	{
		name:  "unbound_oncreate",
		setup: "",
		stmt:  `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET a %s %s`,
		want: wantCounters{
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:  "unbound_onmatch",
		setup: `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		stmt:  `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON MATCH SET a %s %s`,
		want:  wantCounters{propsSet: 1, containsUpdates: true},
	},
	{
		name:  "prebound_oncreate",
		setup: `CREATE (:P {name:'a'}),(:P {name:'b'})`,
		stmt:  `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON CREATE SET a %s %s`,
		want:  wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true},
	},
	{
		name:  "prebound_onmatch",
		setup: `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		stmt:  `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON MATCH SET a %s %s`,
		want:  wantCounters{propsSet: 1, containsUpdates: true},
	},
}

// TestMergeSetAllNode_Matrix is the node-target counterpart of
// TestMergeSetAllRel_Matrix. Only `+=` is exercised: `=` would clear the `name`
// merge key the read-back matches on, which is correct behaviour but a different
// assertion (the node replace path is already pinned by
// TestMergeSetAll_Node_OnCreate_Replace).
func TestMergeSetAllNode_Matrix(t *testing.T) {
	t.Parallel()
	for _, shape := range nodeSetAllShapes {
		for _, rhs := range setAllRHSShapes {
			name := shape.name + "/" + rhs.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				eng := setMapEng(t)
				if shape.setup != "" {
					drainRunInTx(t, eng, shape.setup)
				}
				stmt := fmtStmt(shape.stmt, "+=", rhs.text)
				c := runCountedP(t, eng, stmt, rhs.params)
				assertCounters(t, stmt, c, shape.want)
				if got := setScalarString(t, eng, `MATCH (a:P {name:'a'}) RETURN a.w AS v`); got != "V" {
					t.Fatalf("a.w = %q, want \"V\" after %s", got, stmt)
				}
			})
		}
	}
}

// TestMergeSetAllNode_SecondEndpoint pins the whole-entity action on the SECOND
// fresh endpoint of the chain, the node position the matrix's target `a` does not
// cover. Correct before rmp #2510 and after it: the node arm resolves any chain
// position.
func TestMergeSetAllNode_SecondEndpoint(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng,
		`MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET b += {w:'V'}`)
	if got := setScalarString(t, eng, `MATCH (b:P {name:'b'}) RETURN b.w AS v`); got != "V" {
		t.Fatalf("b.w = %q, want \"V\" (second fresh endpoint)", got)
	}
}

// TestMergeSetAllRel_SingleKeyControl pins the per-property form over unbound
// endpoints, which was correct before rmp #2510 and must stay correct: it
// travels the mergeAction path, not the whole-entity one.
func TestMergeSetAllRel_SingleKeyControl(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	const stmt = `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r.w = 'V'`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, wantCounters{
		nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
	})
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r.w = %q, want \"V\" (per-property control)", got)
	}
}

// TestMergeSetAllRel_PreboundLiteralControl pins the one whole-entity cell that
// was correct before rmp #2510 — both endpoints bound and an all-literal map, the
// only shape extractRelKVActions accepted — so the fix cannot move it off the
// MergeRelationship fast path unnoticed.
func TestMergeSetAllRel_PreboundLiteralControl(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:P {name:'a'}),(:P {name:'b'})`)
	const stmt = `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON CREATE SET r += {w:'V'}`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true})
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r.w = %q, want \"V\" (pre-bound literal control)", got)
	}
}

// TestMergeSetAllRel_Replace_ClearsInlineProps: `=` on a relationship is a
// REPLACE — the inline property the MERGE itself created must be gone, and its
// removal counted as -properties.
func TestMergeSetAllRel_Replace_ClearsInlineProps(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	const stmt = `MERGE (a:P {name:'a'})-[r:T {k:'K'}]->(b:P {name:'b'}) ON CREATE SET r = {w:'V'}`
	c := runCountedP(t, eng, stmt, nil)
	// Three writes create the pattern (two endpoint names + the inline k), then
	// the replace tears k down and writes w.
	assertCounters(t, stmt, c, wantCounters{
		nodesCreated: 2, relsCreated: 1, propsSet: 4, propsRemoved: 1,
		labelsAdded: 2, containsUpdates: true,
	})
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r.w = %q, want \"V\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH ()-[r:T]->() RETURN r.k AS v`) {
		t.Fatal("r.k must be cleared by = replace")
	}
}

// TestMergeSetAllRel_Append_KeepsInlineProps is the `+=` counterpart: the inline
// property survives, since append is purely additive.
func TestMergeSetAllRel_Append_KeepsInlineProps(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng,
		`MERGE (a:P {name:'a'})-[r:T {k:'K'}]->(b:P {name:'b'}) ON CREATE SET r += {w:'V'}`)
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.k AS v`); got != "K" {
		t.Fatalf("r.k = %q, want \"K\" (+= must not clear)", got)
	}
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r.w = %q, want \"V\"", got)
	}
}

// TestMergeSetAllRel_EntityCopy: `ON CREATE SET r = <node>` copies every property
// of the bound entity onto the relationship, the same kind dispatch the node
// whole-entity path performs.
func TestMergeSetAllRel_EntityCopy(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M {w:'V'})`)
	drainRunInTx(t, eng,
		`MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r = m`)
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r.w = %q, want \"V\" (entity copy onto relationship)", got)
	}
}

// TestMergeSetAllRel_NullRHS: `= null` clears the relationship, `+= null` is a
// no-op — openCypher's whole-entity null semantics, matching the node path.
func TestMergeSetAllRel_NullRHS(t *testing.T) {
	t.Parallel()
	t.Run("replace_clears", func(t *testing.T) {
		t.Parallel()
		eng := setMapEng(t)
		drainRunInTx(t, eng,
			`MERGE (a:P {name:'a'})-[r:T {k:'K'}]->(b:P {name:'b'}) ON CREATE SET r = null`)
		if !setScalarIsNull(t, eng, `MATCH ()-[r:T]->() RETURN r.k AS v`) {
			t.Fatal("r.k must be cleared by = null")
		}
	})
	t.Run("append_is_noop", func(t *testing.T) {
		t.Parallel()
		eng := setMapEng(t)
		drainRunInTx(t, eng,
			`MERGE (a:P {name:'a'})-[r:T {k:'K'}]->(b:P {name:'b'}) ON CREATE SET r += null`)
		if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.k AS v`); got != "K" {
			t.Fatalf("r.k = %q, want \"K\" (+= null is a no-op)", got)
		}
	})
}

// TestMergeSetAllRel_ScalarRHS_TypeError: a scalar RHS on a relationship target
// is a TypeError, not a silent no-op — parity with the node path.
func TestMergeSetAllRel_ScalarRHS_TypeError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	err := runDrainErr(t, eng,
		`MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r = size([1,2,3])`)
	if err == nil {
		t.Fatal("ON CREATE SET r = <scalar> must be a TypeError, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("error %q should be a TypeError", err.Error())
	}
}

// TestMergeSetAllRel_OnCreateIsConditional: ON CREATE must not fire when the
// pattern already matched, and ON MATCH must not fire when it was created.
func TestMergeSetAllRel_OnCreateIsConditional(t *testing.T) {
	t.Parallel()
	t.Run("oncreate_silent_on_match", func(t *testing.T) {
		t.Parallel()
		eng := setMapEng(t)
		drainRunInTx(t, eng, `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`)
		const stmt = `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r += {w:'V'}`
		c := runCountedP(t, eng, stmt, nil)
		assertCounters(t, stmt, c, wantCounters{})
		if !setScalarIsNull(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`) {
			t.Fatal("ON CREATE must not fire when the pattern already existed")
		}
	})
	t.Run("onmatch_silent_on_create", func(t *testing.T) {
		t.Parallel()
		eng := setMapEng(t)
		const stmt = `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON MATCH SET r += {w:'V'}`
		c := runCountedP(t, eng, stmt, nil)
		assertCounters(t, stmt, c, wantCounters{ // the two endpoint names only
			nodesCreated: 2, relsCreated: 1, propsSet: 2, labelsAdded: 2, containsUpdates: true,
		})
		if !setScalarIsNull(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`) {
			t.Fatal("ON MATCH must not fire when the pattern was created")
		}
	})
}

// TestMergeSetAllRel_MultiHop: a two-hop chain writes the whole-entity action to
// the hop it names and leaves the other hop untouched.
func TestMergeSetAllRel_MultiHop(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng,
		`MERGE (a:P {name:'a'})-[r1:T]->(b:P {name:'b'})-[r2:U]->(c:P {name:'c'}) ON CREATE SET r2 += {w:'V'}`)
	if got := setScalarString(t, eng, `MATCH ()-[r:U]->() RETURN r.w AS v`); got != "V" {
		t.Fatalf("r2.w = %q, want \"V\" (second hop)", got)
	}
	if !setScalarIsNull(t, eng, `MATCH ()-[r:T]->() RETURN r.w AS v`) {
		t.Fatal("the first hop must not receive the second hop's action")
	}
}

// fmtStmt fills a shape's statement template with the SET operator and the RHS
// text. Kept as a helper so the templates stay readable in the tables above.
func fmtStmt(tmpl, op, rhs string) string {
	out := strings.Replace(tmpl, "%s", op, 1)
	return strings.Replace(out, "%s", rhs, 1)
}

// opName maps a SET operator to a subtest-name fragment (`/` and `=` read badly
// in test names).
func opName(op string) string {
	if op == "=" {
		return "replace"
	}
	return "append"
}
