package cypher_test

// setnull_parallel_edge_counters_test.go — regression gate for #2501: on a
// multigraph, the SET-to-null relationship property removal paths pinned to
// one parallel-edge instance must report PropertiesRemoved per INSTANCE, not
// per (src, dst) pair — the residual of #2500, which fixed the standalone
// REMOVE path only. Before this fix the -properties gate of
// SetProperty.delRelProp (SET r.x = null) and SetAllProperties.deleteOne
// (SET r = {…} / SET r = $map teardown) read the per-pair aggregate store, so
// a removal that strips a property genuinely present on the targeted sibling
// instance counted 0 (or a teardown counted a key the targeted instance never
// carried) — while the by-handle graph STATE was correct throughout.
//
// RED evidence against the old per-pair gate (observed on the pre-fix build):
//   - SET r.since = null on the second sibling reported -properties 0
//     (want 1) on both write paths;
//   - SET r = {eid:1} teardown reported -properties 3 (want 2: `since` was
//     never on the targeted instance's own bag) on both write paths;
//   - SET r = $map teardown reported -properties 3 (want 2) likewise.
//
// Both write paths are covered: the store-less lpgMutatorAdapter and the
// WAL-backed walMutatorAdapter, which carried the same defect.
//
// Layer: short. goleak-clean (engines/graphs are local; the WAL writer is
// closed via t.Cleanup).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// runCountedParams executes a write statement with parameters and returns its
// counters — the params-carrying twin of runCounted.
func runCountedParams(t *testing.T, eng *cypher.Engine, query string, params map[string]expr.Value) *exec.QueryCounters {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, params)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
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

// setNullPerInstanceCounters drives the #2501 shape for `SET r.x = null`
// against eng: two parallel KNOWS instances between the same ordered pair,
// each carrying `since`, then eid-pinned SET-to-null whose -properties must
// follow the TARGETED instance.
func setNullPerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, since:'2026-01-01'}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02'}]->(b)`)

	setNull := func(eid string, want wantCounters) {
		q := `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = ` + eid + ` SET r.since = null`
		assertCounters(t, q, runCounted(t, eng, q), want)
	}

	// First instance: `since` present on eid 1 — exactly one -properties.
	setNull("1", wantCounters{propsRemoved: 1, containsUpdates: true})

	// Same instance again: `since` now absent on eid 1, so this is a no-op
	// and counts nothing — even though the SIBLING (eid 2) still carries it.
	setNull("1", wantCounters{})

	// Sibling instance: `since` genuinely present on eid 2 — one -properties.
	// The defective per-pair gate reported 0 here (observed pre-fix: RED).
	setNull("2", wantCounters{propsRemoved: 1, containsUpdates: true})

	// State cross-check: both instances survive and both `since` values are
	// gone (the counters above are evidence about attribution, not state).
	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.since IS NULL RETURN r.eid`); rows != 2 {
		t.Fatalf("post-SET-null state: %d instances with since IS NULL, want 2", rows)
	}
}

// TestSetNullParallelEdge_InMemory_CountersPerInstance is the #2501 regression
// gate for `SET r.x = null` on the store-less write path (lpgMutatorAdapter).
func TestSetNullParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	setNullPerInstanceCounters(t, counterEngine(t))
}

// TestSetNullParallelEdge_WalStore_CountersPerInstance is the #2501 regression
// gate for `SET r.x = null` on the durable write path (walMutatorAdapter).
func TestSetNullParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	setNullPerInstanceCounters(t, walMultigraphEngine(t))
}

// replaceMapPerInstanceCounters drives the #2501 shape for the whole-entity
// replace teardown (`SET r = {…}` and `SET r = $map`): the pinned instance's
// own bag deliberately lacks `since` while the per-pair aggregate carries it
// (written by the sibling), so a per-pair -properties gate over-counts the
// teardown by exactly one.
func replaceMapPerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	// eid 1 carries NO `since`; eid 2 does — so the pair aggregate holds
	// {eid, since, weight} while eid 1's own bag holds {eid, weight}.
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, weight:1.0}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02', weight:2.0}]->(b)`)

	// Literal-map replace on eid 1: the teardown must count only the keys the
	// TARGETED instance carries (eid, weight → -2), never the sibling's
	// `since` (the defective per-pair gate reported -3 here: RED), plus the
	// one re-written key (+1).
	q := `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r = {eid: 1}`
	assertCounters(t, q, runCounted(t, eng, q), wantCounters{propsRemoved: 2, propsSet: 1, containsUpdates: true})

	// State cross-check: the sibling keeps its own full map.
	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 AND r.since = '2026-02-02' AND r.weight = 2.0 RETURN r.eid`); rows != 1 {
		t.Fatalf("post-replace state: sibling eid 2 property map damaged (rows = %d, want 1)", rows)
	}
}

// TestSetReplaceMapParallelEdge_InMemory_CountersPerInstance is the #2501
// regression gate for `SET r = {…}` teardown on the store-less write path.
func TestSetReplaceMapParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	replaceMapPerInstanceCounters(t, counterEngine(t))
}

// TestSetReplaceMapParallelEdge_WalStore_CountersPerInstance is the #2501
// regression gate for `SET r = {…}` teardown on the durable write path.
func TestSetReplaceMapParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	replaceMapPerInstanceCounters(t, walMultigraphEngine(t))
}

// replaceParamPerInstanceCounters is replaceMapPerInstanceCounters with the
// replacement map supplied as a query parameter (`SET r = $map`), which routes
// through SetAllProperties' parameter-sourced constructor rather than the
// literal-map parser — the second whole-entity teardown path named by #2501.
func replaceParamPerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, weight:1.0}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02', weight:2.0}]->(b)`)

	q := `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r = $map`
	params := map[string]expr.Value{"map": expr.MapValue{"eid": expr.IntegerValue(1)}}
	assertCounters(t, q, runCountedParams(t, eng, q, params),
		wantCounters{propsRemoved: 2, propsSet: 1, containsUpdates: true})

	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 AND r.since = '2026-02-02' AND r.weight = 2.0 RETURN r.eid`); rows != 1 {
		t.Fatalf("post-replace state: sibling eid 2 property map damaged (rows = %d, want 1)", rows)
	}
}

// TestSetReplaceParamParallelEdge_InMemory_CountersPerInstance is the #2501
// regression gate for `SET r = $map` teardown on the store-less write path.
func TestSetReplaceParamParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	replaceParamPerInstanceCounters(t, counterEngine(t))
}

// TestSetReplaceParamParallelEdge_WalStore_CountersPerInstance is the #2501
// regression gate for `SET r = $map` teardown on the durable write path.
func TestSetReplaceParamParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	replaceParamPerInstanceCounters(t, walMultigraphEngine(t))
}

// mergeSetNullPerInstanceCounters probes the MERGE ON MATCH SET null-RHS
// removal route (#2501 suspected path 3). Setup: the pair's FIRST slot (eid 1)
// carries no `since` while the sibling wrote it into the per-pair aggregate —
// a per-pair -properties gate counts a removal although the merge-bound
// instance (the pair's first-slot handle) never carried the key.
func mergeSetNullPerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, weight:1.0}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02', weight:2.0}]->(b)`)

	// Standalone MERGE (MergeRelationship): matches the existing pair, binds
	// the first-slot instance, and the null-evaluating RHS removes `since` —
	// absent on that instance, so nothing may be counted.
	q := `MATCH (a:P {key:'x'}),(b:P {key:'y'}) MERGE (a)-[r:KNOWS]->(b) ON MATCH SET r.since = r.missing`
	assertCounters(t, q, runCounted(t, eng, q), wantCounters{})

	// The sibling's own `since` must be untouched.
	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 AND r.since = '2026-02-02' RETURN r.eid`); rows != 1 {
		t.Fatalf("post-MERGE state: sibling eid 2 lost since (rows = %d, want 1)", rows)
	}
}

// TestMergeSetNullParallelEdge_InMemory_CountersPerInstance is the #2501 probe
// for the MergeRelationship ON MATCH null-RHS removal on the store-less path.
func TestMergeSetNullParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	mergeSetNullPerInstanceCounters(t, counterEngine(t))
}

// TestMergeSetNullParallelEdge_WalStore_CountersPerInstance is the #2501 probe
// for the MergeRelationship ON MATCH null-RHS removal on the durable path.
func TestMergeSetNullParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	mergeSetNullPerInstanceCounters(t, walMultigraphEngine(t))
}

// mergePatternSetNullPerInstanceCounters is the compound-pattern MERGE variant
// (MergePattern.applyRelAction), whose null-RHS remove branch carries the same
// per-pair -properties gate.
func mergePatternSetNullPerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, weight:1.0}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02', weight:2.0}]->(b)`)

	q := `MERGE (a:P {key:'x'})-[r:KNOWS]->(b:P {key:'y'}) ON MATCH SET r.since = r.missing`
	assertCounters(t, q, runCounted(t, eng, q), wantCounters{})

	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 AND r.since = '2026-02-02' RETURN r.eid`); rows != 1 {
		t.Fatalf("post-MERGE state: sibling eid 2 lost since (rows = %d, want 1)", rows)
	}
}

// TestMergePatternSetNullParallelEdge_InMemory_CountersPerInstance is the
// #2501 probe for the compound-pattern MERGE null-RHS removal (store-less).
func TestMergePatternSetNullParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	mergePatternSetNullPerInstanceCounters(t, counterEngine(t))
}

// TestMergePatternSetNullParallelEdge_WalStore_CountersPerInstance is the
// #2501 probe for the compound-pattern MERGE null-RHS removal (durable).
func TestMergePatternSetNullParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	mergePatternSetNullPerInstanceCounters(t, walMultigraphEngine(t))
}
