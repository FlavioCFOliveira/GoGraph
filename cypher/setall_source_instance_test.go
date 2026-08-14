package cypher_test

// setall_source_instance_test.go — adjudication gates for rmp #2503: when the
// SOURCE of a whole-entity assignment (`SET x = r` / `SET x += r`) is a bound
// relationship on a parallel-edge pair, the copied property map must be the
// bound INSTANCE's map — the same map `RETURN properties(r)` shows (reads are
// bag-authoritative since #2502) — never the per-pair AGGREGATE, which can
// carry keys only the parallel twin ever had.
//
// Fixture divergence (the #2502 recipe, twin-pinned): two parallel
// (:P{key:'x'})-[:KNOWS]->(:P{key:'y'}) instances carrying eid 1 and eid 2;
// a SET pinned to eid 2 lands `twinonly` on the aggregate and on eid 2's bag
// only. The aggregate then reads {eid:2, twinonly:7} while properties(r1) is
// {eid:1}. Copying FROM r1 must yield {eid:1}.
//
// Routes, each on both write paths (store-less lpg and WAL-backed):
//  1. SET t = r    — relationship source onto a node target;
//  2. SET s = r    — relationship-to-relationship;
//  3. SET t += r   — append onto a node target;
//  4. SET s += r   — append onto a relationship target;
//  5-8. the four above with a WITH-projected source (`WITH r …`);
//  9. SET t = coalesce(r, null) — the expression route
//     ([exec.SetAllProperties.applyExprValue] RelationshipValue arm).
//
// Layer: short. goleak-clean (engines/graphs are local; the WAL writer is
// closed via t.Cleanup).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// seedSourceInstanceFixture builds the parallel pair with the twin-pinned
// aggregate divergence, plus a node target (:T {tk:1}) and a relationship
// target (:Q {qk:'c'})-[:LINKS {sid:9}]->(:Q {qk:'d'}) on a separate pair.
func seedSourceInstanceFixture(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	twoParallelKnows(t, eng)
	// Pinned to the twin: lands on the aggregate and on eid 2's bag only.
	mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 SET r.twinonly = 7`)
	mustRunWrite(t, eng, `CREATE (:T {tk: 1})`)
	mustRunWrite(t, eng, `CREATE (:Q {qk:'c'})-[:LINKS {sid: 9}]->(:Q {qk:'d'})`)
}

// nodeTargetProps reads back the node target's full property map.
func nodeTargetProps(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	got := scalarRows(t, eng, `MATCH (t:T) RETURN properties(t) AS v`, "v")
	if len(got) != 1 {
		t.Fatalf("node target: %d rows, want 1: %v", len(got), got)
	}
	return got[0]
}

// relTargetProps reads back the relationship target's full property map.
func relTargetProps(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	got := scalarRows(t, eng, `MATCH (:Q {qk:'c'})-[s:LINKS]->(:Q {qk:'d'}) RETURN properties(s) AS v`, "v")
	if len(got) != 1 {
		t.Fatalf("relationship target: %d rows, want 1: %v", len(got), got)
	}
	return got[0]
}

// assertSourcesUntouched pins the source pair's post-copy state: the copy must
// not disturb either parallel instance.
func assertSourcesUntouched(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
		t.Fatalf("source r1 disturbed: properties(r1) = %s, want {eid=i:1}", got)
	}
	if got := propsOf(t, eng, "2"); got != "{eid=i:2,twinonly=i:7}" {
		t.Fatalf("source r2 disturbed: properties(r2) = %s, want {eid=i:2,twinonly=i:7}", got)
	}
}

// setallSourceRoute runs one copy-from-relationship route: seed, execute the
// SET statement, and require the target's map to equal want (the bound
// instance's map, exactly as properties(r) reports it).
func setallSourceRoute(t *testing.T, query, want string, readTarget func(*testing.T, *cypher.Engine) string) {
	t.Helper()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		seedSourceInstanceFixture(t, eng)
		mustRunWrite(t, eng, query)
		if got := readTarget(t, eng); got != want {
			t.Fatalf("after %q, target properties = %s, want %s (aggregate pair map leaked the twin's keys)", query, got, want)
		}
		assertSourcesUntouched(t, eng)
	})
}

// TestSetAllSourceInstance_NodeTargetReplace is route 1: SET t = r.
func TestSetAllSourceInstance_NodeTargetReplace(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 MATCH (t:T) SET t = r`,
		"{eid=i:1}", nodeTargetProps)
}

// TestSetAllSourceInstance_RelTargetReplace is route 2: SET s = r.
func TestSetAllSourceInstance_RelTargetReplace(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 MATCH (:Q {qk:'c'})-[s:LINKS]->(:Q {qk:'d'}) SET s = r`,
		"{eid=i:1}", relTargetProps)
}

// TestSetAllSourceInstance_NodeTargetAppend is route 3: SET t += r.
func TestSetAllSourceInstance_NodeTargetAppend(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 MATCH (t:T) SET t += r`,
		"{eid=i:1,tk=i:1}", nodeTargetProps)
}

// TestSetAllSourceInstance_RelTargetAppend is route 4: SET s += r.
func TestSetAllSourceInstance_RelTargetAppend(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 MATCH (:Q {qk:'c'})-[s:LINKS]->(:Q {qk:'d'}) SET s += r`,
		"{eid=i:1,sid=i:9}", relTargetProps)
}

// TestSetAllSourceInstance_WithProjected_NodeTargetReplace is route 5:
// WITH r … SET t = r (post-projection source carries the handle in the
// RelationshipValue's ID).
func TestSetAllSourceInstance_WithProjected_NodeTargetReplace(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 WITH r MATCH (t:T) SET t = r`,
		"{eid=i:1}", nodeTargetProps)
}

// TestSetAllSourceInstance_WithProjected_RelTargetReplace is route 6:
// WITH r … SET s = r.
func TestSetAllSourceInstance_WithProjected_RelTargetReplace(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 WITH r MATCH (:Q {qk:'c'})-[s:LINKS]->(:Q {qk:'d'}) SET s = r`,
		"{eid=i:1}", relTargetProps)
}

// TestSetAllSourceInstance_WithProjected_NodeTargetAppend is route 7:
// WITH r … SET t += r.
func TestSetAllSourceInstance_WithProjected_NodeTargetAppend(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 WITH r MATCH (t:T) SET t += r`,
		"{eid=i:1,tk=i:1}", nodeTargetProps)
}

// TestSetAllSourceInstance_WithProjected_RelTargetAppend is route 8:
// WITH r … SET s += r.
func TestSetAllSourceInstance_WithProjected_RelTargetAppend(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 WITH r MATCH (:Q {qk:'c'})-[s:LINKS]->(:Q {qk:'d'}) SET s += r`,
		"{eid=i:1,sid=i:9}", relTargetProps)
}

// TestSetAllSourceInstance_ExprValueRelationship is route 9: the whole-entity
// SET whose RHS is a general expression evaluating to a relationship
// ([exec.SetAllProperties.applyExprValue] RelationshipValue arm). Since rmp
// #2317 the evaluated value's ID IS the stable handle, so the copy must be
// bag-routed exactly like the bound-variable form.
func TestSetAllSourceInstance_ExprValueRelationship(t *testing.T) {
	t.Parallel()
	setallSourceRoute(t,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 WITH r MATCH (t:T) SET t = coalesce(r, null)`,
		"{eid=i:1}", nodeTargetProps)
}
