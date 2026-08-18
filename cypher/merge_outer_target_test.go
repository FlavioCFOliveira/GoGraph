package cypher_test

// merge_outer_target_test.go — regression coverage for an ON CREATE / ON MATCH
// SET whose target variable is bound by a clause PRECEDING the MERGE, rather
// than by the MERGE pattern itself (rmp #2511).
//
//	MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m.w = 'V'
//
// silently dropped the write: the statement reported success, the pattern was
// created, `m.w` read back null, and no +properties was counted for it. Both
// action paths of the MergePattern operator resolved the target only inside the
// merge pattern's own variable maps — nodeIndexByVar over the chain positions and
// hopIndexByRelVar over the chain hops — and skipped anything else as "not a
// target this operator can resolve". An outer variable is exactly such a target,
// so the action had no writer at all.
//
// Routing, established by EXPLAIN rather than assumed:
//
//   - Every shape below with a relationship hop routes to MergePattern, whether
//     the endpoints are fresh or pre-bound. The narrower MergeRelationship fast
//     path cannot receive an outer-variable action at all: extractRelKVActions
//     (cypher/ir/writes.go) rejects any item whose target is not the relationship
//     variable, and that rejection is what sends the statement to MergePattern.
//   - The node-only Merge operator already resolved a NODE target through the
//     full row schema (resolveNodeIDFromRow), so an outer node target was never
//     lost there — it is the reference behaviour this fix brings MergePattern up
//     to. An outer RELATIONSHIP target was lost there too, for the same reason in
//     miniature: resolveActionNodeKey only understands node values.
//
// The matrix crosses target kind (outer node, outer relationship) × action form
// (per-property, whole-entity `+=`, whole-entity `=`) × pattern shape (fresh
// endpoints, pre-bound endpoints, node-only MERGE) × branch (ON CREATE,
// ON MATCH). The in-pattern controls that were always correct are pinned
// separately so the fix cannot move them.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// outerActionForm is one surface form of a SET action, rendered against the
// variable it targets. All three write the single property w='V'.
type outerActionForm struct {
	render func(v string) string
	name   string
}

// outerActionForms are the three shapes an ON CREATE / ON MATCH SET item takes.
// The per-property form travels the mergeAction path; the two whole-entity forms
// travel the MergeSetAllAction path. Before rmp #2511 all three lost an outer
// target, which is what distinguishes this defect from #2510 (whole-entity only).
var outerActionForms = []outerActionForm{
	{name: "per_property", render: func(v string) string { return v + ".w = 'V'" }},
	{name: "append", render: func(v string) string { return v + " += {w:'V'}" }},
	{name: "replace", render: func(v string) string { return v + " = {w:'V'}" }},
}

// outerTargetKind is the entity bound by the preceding clause that the action
// writes to. Both entities are seeded WITHOUT properties, so `=` (replace) and
// `+=` (append) reach the same final state and share one counter expectation.
type outerTargetKind struct {
	name      string
	varName   string
	setup     string
	matchFrag string
	readback  string
}

var outerTargetKinds = []outerTargetKind{
	{
		name:      "node",
		varName:   "m",
		setup:     `CREATE (:M)`,
		matchFrag: `(m:M)`,
		readback:  `MATCH (m:M) RETURN m.w AS v`,
	},
	{
		name:      "relationship",
		varName:   "rr",
		setup:     `CREATE (:S)-[:U]->(:E)`,
		matchFrag: `(s)-[rr:U]->(e)`,
		readback:  `MATCH ()-[rr:U]->() RETURN rr.w AS v`,
	},
}

// outerShape is one MERGE shape × branch: the graph state the branch needs, any
// extra MATCH items that pre-bind the pattern's endpoints, the MERGE clause up to
// and including the branch keyword, and the side effects the statement must
// report. want.propsSet counts the properties the MERGE itself writes plus the
// one the outer-target SET must write.
type outerShape struct {
	name       string
	setup      string
	matchExtra string
	merge      string
	want       wantCounters
}

// outerShapes covers all three merge operators an outer-target action can reach:
// MergePattern with fresh endpoints, MergePattern with pre-bound endpoints, and
// the node-only Merge.
var outerShapes = []outerShape{
	{
		name:  "pattern_fresh/oncreate",
		merge: `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE`,
		want: wantCounters{ // two endpoint names + w, plus the two :P labels
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:  "pattern_fresh/onmatch",
		setup: `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		merge: `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON MATCH`,
		want:  wantCounters{propsSet: 1, containsUpdates: true},
	},
	{
		name:       "pattern_bound/oncreate",
		setup:      `CREATE (:P {name:'a'}),(:P {name:'b'})`,
		matchExtra: `(a:P {name:'a'}),(b:P {name:'b'})`,
		merge:      `MERGE (a)-[r:T]->(b) ON CREATE`,
		want:       wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true},
	},
	{
		name:       "pattern_bound/onmatch",
		setup:      `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`,
		matchExtra: `(a:P {name:'a'}),(b:P {name:'b'})`,
		merge:      `MERGE (a)-[r:T]->(b) ON MATCH`,
		want:       wantCounters{propsSet: 1, containsUpdates: true},
	},
	{
		name:  "node_only/oncreate",
		merge: `MERGE (z:P {name:'z'}) ON CREATE`,
		want: wantCounters{ // z's name + w, plus the :P label
			nodesCreated: 1, propsSet: 2, labelsAdded: 1, containsUpdates: true,
		},
	},
	{
		name:  "node_only/onmatch",
		setup: `CREATE (:P {name:'z'})`,
		merge: `MERGE (z:P {name:'z'}) ON MATCH`,
		want:  wantCounters{propsSet: 1, containsUpdates: true},
	},
}

// outerStmt assembles the statement for one (target, shape, form) cell.
func outerStmt(tk *outerTargetKind, sh *outerShape, form outerActionForm) string {
	match := "MATCH " + tk.matchFrag
	if sh.matchExtra != "" {
		match += "," + sh.matchExtra
	}
	return match + " " + sh.merge + " SET " + form.render(tk.varName)
}

// TestMergeOuterTarget_Matrix is the outer-variable action matrix: every target
// kind × shape × form must write the property it names and count it. Before rmp
// #2511 every cell whose statement reached MergePattern reported success while
// writing nothing, as did every relationship-target cell on the node-only Merge.
func TestMergeOuterTarget_Matrix(t *testing.T) {
	t.Parallel()
	for _, tk := range outerTargetKinds {
		for _, sh := range outerShapes {
			for _, form := range outerActionForms {
				name := tk.name + "/" + sh.name + "/" + form.name
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					eng := setMapEng(t)
					drainRunInTx(t, eng, tk.setup)
					if sh.setup != "" {
						drainRunInTx(t, eng, sh.setup)
					}
					stmt := outerStmt(&tk, &sh, form)
					c := runCountedP(t, eng, stmt, nil)
					assertCounters(t, stmt, c, sh.want)
					if got := setScalarString(t, eng, tk.readback); got != "V" {
						t.Fatalf("%s.w = %q, want \"V\" after %s", tk.varName, got, stmt)
					}
				})
			}
		}
	}
}

// TestMergeOuterTarget_BranchIsConditional: an outer-target action must obey the
// branch it is attached to — ON CREATE stays silent when the pattern matched, and
// ON MATCH stays silent when it was created. Without this a fix that simply
// applies every action unconditionally would pass the matrix above.
func TestMergeOuterTarget_BranchIsConditional(t *testing.T) {
	t.Parallel()
	for _, form := range outerActionForms {
		t.Run("oncreate_silent_on_match/"+form.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			drainRunInTx(t, eng, `CREATE (:M)`)
			drainRunInTx(t, eng, `CREATE (:P {name:'a'})-[:T]->(:P {name:'b'})`)
			stmt := `MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET ` + form.render("m")
			c := runCountedP(t, eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{})
			if !setScalarIsNull(t, eng, `MATCH (m:M) RETURN m.w AS v`) {
				t.Fatal("ON CREATE must not fire when the pattern already existed")
			}
		})
		t.Run("onmatch_silent_on_create/"+form.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			drainRunInTx(t, eng, `CREATE (:M)`)
			stmt := `MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON MATCH SET ` + form.render("m")
			c := runCountedP(t, eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{ // the two endpoint names only
				nodesCreated: 2, relsCreated: 1, propsSet: 2, labelsAdded: 2, containsUpdates: true,
			})
			if !setScalarIsNull(t, eng, `MATCH (m:M) RETURN m.w AS v`) {
				t.Fatal("ON MATCH must not fire when the pattern was created")
			}
		})
	}
}

// TestMergeOuterTarget_MixedWithPatternTarget: one branch carrying both an
// in-pattern target and an outer target must apply both. This is also the
// statement shape that proves the routing claim in the file header — the outer
// item is what rejects the MergeRelationship fast path, so the in-pattern
// relationship item travels MergePattern with it.
func TestMergeOuterTarget_MixedWithPatternTarget(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M)`)
	drainRunInTx(t, eng, `CREATE (:P {name:'a'}),(:P {name:'b'})`)
	const stmt = `MATCH (m:M),(a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ` +
		`ON CREATE SET r.k = 'K', m.w = 'V'`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, wantCounters{relsCreated: 1, propsSet: 2, containsUpdates: true})
	if got := setScalarString(t, eng, `MATCH ()-[r:T]->() RETURN r.k AS v`); got != "K" {
		t.Fatalf("r.k = %q, want \"K\" (in-pattern target in a mixed branch)", got)
	}
	if got := setScalarString(t, eng, `MATCH (m:M) RETURN m.w AS v`); got != "V" {
		t.Fatalf("m.w = %q, want \"V\" (outer target in a mixed branch)", got)
	}
}

// TestMergeOuterTarget_NullTargetIsNoOp: an outer variable that resolved to null
// is a silent no-op, per openCypher's rule that mutating clauses ignore null
// inputs. The fix must turn "target not found" into a write only when the target
// really resolves — never into an error on a null binding.
func TestMergeOuterTarget_NullTargetIsNoOp(t *testing.T) {
	t.Parallel()
	for _, form := range outerActionForms {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			stmt := `OPTIONAL MATCH (m:NOPE) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET ` +
				form.render("m")
			c := runCountedP(t, eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{
				nodesCreated: 2, relsCreated: 1, propsSet: 2, labelsAdded: 2, containsUpdates: true,
			})
		})
	}
}

// TestMergeOuterTarget_ExpressionRHS: an outer target whose right-hand side is a
// non-literal expression must be evaluated against the same row, not dropped as a
// literal-parse failure. `m.n + 'X'` reads the outer node's own current value, so
// a fix that wrote a constant would not satisfy it.
func TestMergeOuterTarget_ExpressionRHS(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M {n: 'A'})`)
	const stmt = `MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m.n = m.n + 'X'`
	drainRunInTx(t, eng, stmt)
	if got := setScalarString(t, eng, `MATCH (m:M) RETURN m.n AS v`); got != "AX" {
		t.Fatalf("m.n = %q, want \"AX\" (expression RHS on an outer target)", got)
	}
}

// TestMergeOuterTarget_LabelAction: `ON CREATE SET m:L` on an outer node adds the
// label and counts it, exactly as the in-pattern label action already did.
func TestMergeOuterTarget_LabelAction(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M)`)
	const stmt = `MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m:Tagged`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, wantCounters{
		nodesCreated: 2, relsCreated: 1, propsSet: 2, labelsAdded: 3, containsUpdates: true,
	})
	if n := outerRowCount(t, eng, `MATCH (m:Tagged) RETURN m AS v`); n != 1 {
		t.Fatalf("MATCH (m:Tagged) returned %d rows, want 1", n)
	}
}

// TestMergeOuterTarget_EntityCopy: `ON CREATE SET m = a` copies every property of
// an in-pattern node onto an outer node — the whole-entity kind dispatch reached
// through the outer arm.
func TestMergeOuterTarget_EntityCopy(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M)`)
	drainRunInTx(t, eng,
		`MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m = a`)
	if got := setScalarString(t, eng, `MATCH (m:M) RETURN m.name AS v`); got != "a" {
		t.Fatalf("m.name = %q, want \"a\" (entity copy onto an outer node)", got)
	}
}

// TestMergeOuterTarget_ReplaceClearsOuterProps: `=` on an outer node is a REPLACE
// — the property it already carried must be gone, and its removal counted.
func TestMergeOuterTarget_ReplaceClearsOuterProps(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M {k:'K'})`)
	const stmt = `MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m = {w:'V'}`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, wantCounters{
		nodesCreated: 2, relsCreated: 1, propsSet: 3, propsRemoved: 1,
		labelsAdded: 2, containsUpdates: true,
	})
	if !setScalarIsNull(t, eng, `MATCH (m:M) RETURN m.k AS v`) {
		t.Fatal("m.k must be cleared by = replace on an outer node")
	}
	if got := setScalarString(t, eng, `MATCH (m:M) RETURN m.w AS v`); got != "V" {
		t.Fatalf("m.w = %q, want \"V\"", got)
	}
}

// TestMergeOuterTarget_ScalarRHS_TypeError: a scalar RHS on an outer target is a
// TypeError, not a silent no-op — parity with the in-pattern target path.
func TestMergeOuterTarget_ScalarRHS_TypeError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M)`)
	err := runDrainErr(t, eng,
		`MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m = size([1,2,3])`)
	if err == nil {
		t.Fatal("ON CREATE SET m = <scalar> must be a TypeError, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("error %q should be a TypeError", err.Error())
	}
}

// TestMergeOuterTarget_UnboundDriverIsNotAPanic: when the driving MATCH binds
// nothing, the row the MERGE fires against leaves the outer variable's slot UNSET
// — a plain nil interface, not the null singleton — and resolving an action
// against it used to dereference that nil inside expr.IsNull and abort the whole
// statement with an internal panic.
//
// The target is unresolvable, so the action writes nothing; the assertion is that
// it is a silent no-op rather than a crash. Deliberately NOT asserted here: that
// the MERGE pattern is created at all after a MATCH that produced no rows. That
// is a separate defect from rmp #2511 — openCypher runs MERGE once per incoming
// row, so zero rows must run it zero times — and pinning it either way would make
// this test change when that one is fixed.
func TestMergeOuterTarget_UnboundDriverIsNotAPanic(t *testing.T) {
	t.Parallel()
	for _, form := range outerActionForms {
		for _, merge := range []string{
			`MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'})`,
			`MERGE (z:P {name:'z'})`,
		} {
			t.Run(form.name+"/"+merge[:12], func(t *testing.T) {
				t.Parallel()
				eng := setMapEng(t)
				drainRunInTx(t, eng, `CREATE (:M {name:'present'})`)
				// No :M carries 'absent', so the MATCH binds nothing.
				stmt := `MATCH (m:M {name:'absent'}) ` + merge + ` ON CREATE SET ` + form.render("m")
				if err := runDrainErr(t, eng, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
				if !setScalarIsNull(t, eng, `MATCH (m:M {name:'present'}) RETURN m.w AS v`) {
					t.Fatal("an unbound outer target must not write to some other node")
				}
			})
		}
	}
}

// inPatternControl is one statement whose action target belongs to the MERGE
// pattern itself. Every one of these was correct before rmp #2511 and must stay
// byte-identical after it: the outer arm is reached only when the chain arms miss.
type inPatternControl struct {
	name     string
	setup    string
	stmt     string
	readback string
	want     wantCounters
}

var inPatternControls = []inPatternControl{
	{
		name:     "fresh_node_per_property",
		stmt:     `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET a.w = 'V'`,
		readback: `MATCH (a:P {name:'a'}) RETURN a.w AS v`,
		want: wantCounters{
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:     "fresh_node_append",
		stmt:     `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET a += {w:'V'}`,
		readback: `MATCH (a:P {name:'a'}) RETURN a.w AS v`,
		want: wantCounters{
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:     "fresh_rel_per_property",
		stmt:     `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r.w = 'V'`,
		readback: `MATCH ()-[r:T]->() RETURN r.w AS v`,
		want: wantCounters{
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:     "fresh_rel_append",
		stmt:     `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET r += {w:'V'}`,
		readback: `MATCH ()-[r:T]->() RETURN r.w AS v`,
		want: wantCounters{
			nodesCreated: 2, relsCreated: 1, propsSet: 3, labelsAdded: 2, containsUpdates: true,
		},
	},
	{
		name:     "bound_node_per_property",
		setup:    `CREATE (:P {name:'a'}),(:P {name:'b'})`,
		stmt:     `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON CREATE SET a.w = 'V'`,
		readback: `MATCH (a:P {name:'a'}) RETURN a.w AS v`,
		want:     wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true},
	},
	{
		name:     "bound_rel_append",
		setup:    `CREATE (:P {name:'a'}),(:P {name:'b'})`,
		stmt:     `MATCH (a:P {name:'a'}),(b:P {name:'b'}) MERGE (a)-[r:T]->(b) ON CREATE SET r += {w:'V'}`,
		readback: `MATCH ()-[r:T]->() RETURN r.w AS v`,
		want:     wantCounters{relsCreated: 1, propsSet: 1, containsUpdates: true},
	},
	{
		name:     "node_only_per_property",
		stmt:     `MERGE (z:P {name:'z'}) ON CREATE SET z.w = 'V'`,
		readback: `MATCH (z:P {name:'z'}) RETURN z.w AS v`,
		want: wantCounters{
			nodesCreated: 1, propsSet: 2, labelsAdded: 1, containsUpdates: true,
		},
	},
	{
		name:     "node_only_append",
		stmt:     `MERGE (z:P {name:'z'}) ON CREATE SET z += {w:'V'}`,
		readback: `MATCH (z:P {name:'z'}) RETURN z.w AS v`,
		want: wantCounters{
			nodesCreated: 1, propsSet: 2, labelsAdded: 1, containsUpdates: true,
		},
	},
}

// TestMergeOuterTarget_InPatternControls pins the cells the outer-target fix must
// not disturb: every action whose target is a variable the MERGE pattern itself
// introduces or binds.
func TestMergeOuterTarget_InPatternControls(t *testing.T) {
	t.Parallel()
	for _, c := range inPatternControls {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			if c.setup != "" {
				drainRunInTx(t, eng, c.setup)
			}
			got := runCountedP(t, eng, c.stmt, nil)
			assertCounters(t, c.stmt, got, c.want)
			if v := setScalarString(t, eng, c.readback); v != "V" {
				t.Fatalf("%s: read back %q, want \"V\"", c.stmt, v)
			}
		})
	}
}

// outerRowCount runs query and returns the number of rows it produces.
func outerRowCount(t *testing.T, eng *cypher.Engine, query string) int {
	t.Helper()
	res, err := eng.Run(t.Context(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return n
}
