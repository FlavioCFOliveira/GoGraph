package cypher_test

// setall_parallel_edge_state_test.go — regression gates for the two STATE
// defects adjudicated by rmp #2502 on the whole-entity relationship SET
// (`SET r = {…}` / `SET r += {…}`) over parallel-edge instances:
//
//  1. Replace teardown must clear INSTANCE-ONLY keys. The `SET r = map`
//     contract is "the resulting property map equals exactly the given map",
//     but the teardown enumerated the keys of the per-pair AGGREGATE store
//     only. A key present in the targeted instance's by-handle bag yet absent
//     from the aggregate (because a REMOVE pinned to the parallel twin
//     deleted the pair-level entry) survived the replace — and the read path
//     is bag-authoritative, so the stale key was user-visible.
//
//  2. The stable handle must survive a WITH/projection boundary. The
//     SetAllProperties entity resolver left relHandle at 0 for a
//     post-projection RelationshipValue (whose ID IS the stable handle since
//     rmp #2317), silently degrading `WITH r SET r = {…}` / `+= {…}` to the
//     pairwise path: the per-pair store changed while the instance's bag —
//     which reads route through — kept the pre-SET map.
//
// Both write paths are covered: the store-less lpgMutatorAdapter and the
// WAL-backed walMutatorAdapter.
//
// Layer: short. goleak-clean (engines/graphs are local; the WAL writer is
// closed via t.Cleanup).

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// writeRows runs a WRITE statement to completion inside a transaction and
// returns column col of every returned row rendered via renderExpr, sorted.
// It is the write-path sibling of scalarRows (which runs through RunAny):
// a `SET … RETURN …` statement must observe its own writes.
func writeRows(t *testing.T, eng *cypher.Engine, query, col string) []string {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
	}
	var got []string
	for res.Next() {
		got = append(got, renderExpr(res.Record()[col]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", query, err)
	}
	sort.Strings(got)
	return got
}

// twoParallelKnows builds (:P {key:'x'})-[:KNOWS {eid:1}]->(:P {key:'y'}) and
// a parallel twin with eid:2, each instance carrying ONLY its eid.
func twoParallelKnows(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	mustRunWrite(t, eng, `CREATE (:P {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (:P {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2}]->(b)`)
}

// propsOf reads back the full property map of the instance pinned by eid.
func propsOf(t *testing.T, eng *cypher.Engine, eid string) string {
	t.Helper()
	got := scalarRows(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = `+eid+` RETURN properties(r) AS v`, "v")
	if len(got) != 1 {
		t.Fatalf("eid=%s: %d rows, want 1 (parallel instance lost or duplicated): %v", eid, len(got), got)
	}
	return got[0]
}

// setallEngines runs fn against a fresh engine per write path.
func setallEngines(t *testing.T, fn func(t *testing.T, eng *cypher.Engine)) {
	t.Helper()
	t.Run("InMemory", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		fn(t, cypher.NewEngine(g))
	})
	t.Run("WalStore", func(t *testing.T) {
		t.Parallel()
		fn(t, walMultigraphEngine(t))
	})
}

// TestSetAllParallelEdge_ReplaceTearsDownInstanceOnlyKey is the suspicion-1
// gate (#2502): a key carried ONLY by the targeted instance's by-handle bag —
// the per-pair aggregate entry was deleted by a REMOVE pinned to the twin —
// must still be torn down by `SET r = {…}` replace.
func TestSetAllParallelEdge_ReplaceTearsDownInstanceOnlyKey(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		// `only` lands on both instances' bags and on the pair aggregate…
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r.only = 1`)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 SET r.only = 2`)
		// …then the twin-pinned REMOVE deletes the AGGREGATE entry (and the
		// twin's bag), leaving `only` instance-only on eid 1.
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 REMOVE r.only`)

		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r = {eid: 1}`)

		if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
			t.Fatalf("after SET r = {eid: 1}, properties(r1) = %s, want {eid=i:1} (instance-only key survived the replace)", got)
		}
		if got := propsOf(t, eng, "2"); got != "{eid=i:2}" {
			t.Fatalf("twin corrupted: properties(r2) = %s, want {eid=i:2}", got)
		}
	})
}

// TestSetAllParallelEdge_ReplaceTearsDownKeyTwinNeverHad is the suspicion-1
// variant where the twin NEVER carried the key: a REMOVE pinned to the twin is
// an openCypher no-op on the twin, yet it still clears the pair-level
// aggregate entry, diverging aggregate from instance state.
func TestSetAllParallelEdge_ReplaceTearsDownKeyTwinNeverHad(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r.only = 1`)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 REMOVE r.only`)

		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r = {eid: 1}`)

		if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
			t.Fatalf("after SET r = {eid: 1}, properties(r1) = %s, want {eid=i:1} (instance-only key survived the replace)", got)
		}
		if got := propsOf(t, eng, "2"); got != "{eid=i:2}" {
			t.Fatalf("twin corrupted: properties(r2) = %s, want {eid=i:2}", got)
		}
	})
}

// TestSetAllParallelEdge_WithProjection_Replace is the suspicion-2 gate for
// `=` (#2502, class #2334): a whole-entity replace issued after a WITH
// boundary must mutate the SAME instance a read resolves — its by-handle bag —
// not silently degrade to the pairwise path.
func TestSetAllParallelEdge_WithProjection_Replace(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r = {eid: 2, y: 9}`)

		if got := propsOf(t, eng, "2"); got != "{eid=i:2,y=i:9}" {
			t.Fatalf("after WITH r SET r = {eid: 2, y: 9}, properties(r2) = %s, want {eid=i:2,y=i:9}", got)
		}
		if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
			t.Fatalf("sibling corrupted: properties(r1) = %s, want {eid=i:1}", got)
		}
	})
}

// TestSetAllParallelEdge_WithProjection_Append is the suspicion-2 gate for
// `+=`: the appended key must land on the projected instance's own bag.
func TestSetAllParallelEdge_WithProjection_Append(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r += {z: 5}`)

		if got := propsOf(t, eng, "2"); got != "{eid=i:2,z=i:5}" {
			t.Fatalf("after WITH r SET r += {z: 5}, properties(r2) = %s, want {eid=i:2,z=i:5}", got)
		}
		if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
			t.Fatalf("sibling corrupted: properties(r1) = %s, want {eid=i:1}", got)
		}
	})
}

// TestSetAllParallelEdge_WithProjection_ReadYourOwnWrites pins the row-refresh
// after a post-projection whole-entity SET: the SAME statement's RETURN must
// observe the replaced map, not the projection's pre-SET snapshot.
func TestSetAllParallelEdge_WithProjection_ReadYourOwnWrites(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		got := writeRows(t, eng,
			`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r = {eid: 2, y: 9} RETURN properties(r) AS v`, "v")
		if len(got) != 1 || got[0] != "{eid=i:2,y=i:9}" {
			t.Fatalf("same-statement RETURN properties(r) = %v, want [{eid=i:2,y=i:9}]", got)
		}
	})
}

// TestSetAllParallelEdge_WithProjection_AppendReadYourOwnWrites pins the
// row-refresh routing: after a post-projection `+=` the SAME statement's
// RETURN must observe the INSTANCE's map, not the per-pair aggregate — which
// still carries the twin's keys (`only` lives on eid 1 alone here).
func TestSetAllParallelEdge_WithProjection_AppendReadYourOwnWrites(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 1 SET r.only = 1`)
		got := writeRows(t, eng,
			`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r += {z: 5} RETURN properties(r) AS v`, "v")
		if len(got) != 1 || got[0] != "{eid=i:2,z=i:5}" {
			t.Fatalf("same-statement RETURN properties(r) = %v, want [{eid=i:2,z=i:5}] (aggregate leaked the twin's keys)", got)
		}
	})
}

// TestSetParallelEdge_WithProjection_SingleKey pins the ALREADY-CORRECT
// behaviour of the per-property SET operator across a WITH boundary (fixed by
// rmp #2334): both the assignment and the SET-to-null removal reach the
// projected instance's own bag. Kept as the exoneration guard for the
// SetProperty half of suspicion 2 (#2502).
func TestSetParallelEdge_WithProjection_SingleKey(t *testing.T) {
	t.Parallel()
	setallEngines(t, func(t *testing.T, eng *cypher.Engine) {
		twoParallelKnows(t, eng)
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r.x = 7`)
		if got := propsOf(t, eng, "2"); got != "{eid=i:2,x=i:7}" {
			t.Fatalf("after WITH r SET r.x = 7, properties(r2) = %s, want {eid=i:2,x=i:7}", got)
		}
		mustRunWrite(t, eng, `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = 2 WITH r SET r.x = null`)
		if got := propsOf(t, eng, "2"); got != "{eid=i:2}" {
			t.Fatalf("after WITH r SET r.x = null, properties(r2) = %s, want {eid=i:2}", got)
		}
		if got := propsOf(t, eng, "1"); got != "{eid=i:1}" {
			t.Fatalf("sibling corrupted: properties(r1) = %s, want {eid=i:1}", got)
		}
	})
}
