package cypher_test

// merge_outer_rel_handle_test.go — regression coverage for a MERGE ON CREATE /
// ON MATCH action whose target is an OUTER RELATIONSHIP whose stable handle
// collides with an existing node's id (rmp #2515).
//
//	MATCH (s)-[rr:U]->(e) MERGE (z:P {name:'z'}) ON MATCH SET rr.w = 'V'
//
// wrote `w='V'` onto an unrelated NODE and left the relationship untouched, while
// reporting +properties = 1. The two namespaces share a representation: since rmp
// #2317 a relationship rides in the row as a bare [expr.IntegerValue] holding the
// instance's stable HANDLE, and [resolveNodeIDFromRow] converts an IntegerValue to
// a [graph.NodeID] unconditionally. The node-only Merge operator asked that
// question FIRST, so whenever the graph happened to contain a node whose id
// equalled the handle, the node arm answered — and answered wrong. Only when it
// found no such node did the outer-relationship arm added by rmp #2511 get its
// turn, which is why every other graph wrote to the right entity.
//
// That makes the defect a misdirected write, not merely a lost one: the property
// lands on a real, unrelated entity and the statement reports success.
//
// # Why this test is deterministic and the matrix in merge_outer_target_test.go
// is not
//
// Node ids are allocated per mapper shard, so they are a deterministic but
// scattered function of the synthetic node key — which is drawn from a
// process-global counter. Whether any node's id happens to equal the edge's handle
// therefore varies from one engine to the next: TestMergeOuterTarget_Matrix
// reproduced this on roughly 1% of its runs, which is a defect report rather than
// a gate.
//
// Here the collision is CONSTRUCTED instead of awaited. [lpg.Graph.SeedEdgeHandle]
// raises the per-graph handle counter, so the next relationship the engine creates
// is stamped with a handle chosen by the test — the id of a decoy node created
// beforehand. The collision precondition is then VERIFIED against
// [lpg.Graph.FirstEdgeHandle] rather than assumed, so the test cannot silently
// decay into passing for the wrong reason if handle allocation ever changes.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// handleCollisionFixture is a graph in which the single `:U` relationship's
// stable handle equals the id of a node labelled `:DECOY`.
type handleCollisionFixture struct {
	eng    *cypher.Engine
	handle uint64
}

// newHandleCollisionFixture builds that graph. The decoy is created first so its
// id is known, the handle counter is seeded to that id, and only then is the
// relationship created — so the engine itself stamps the colliding handle through
// its ordinary write path rather than the test reaching past it.
func newHandleCollisionFixture(t *testing.T) *handleCollisionFixture {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	// Two decoys: node ids start at 0 and 0 is the reserved "no handle" sentinel,
	// so at least one of the two is a usable handle value.
	drainRunInTx(t, eng, `CREATE (:DECOY),(:DECOY)`)
	var decoyID graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if id > decoyID {
			decoyID = id
		}
		return true
	})
	if decoyID == 0 {
		t.Fatal("no decoy node received a non-zero id; the collision cannot be constructed")
	}

	g.SeedEdgeHandle(uint64(decoyID))
	drainRunInTx(t, eng, `CREATE (:S)-[:U]->(:E)`)

	// Verify the precondition rather than assume it: without the collision this
	// test proves nothing at all.
	var srcKey, dstKey string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		for _, l := range g.NodeLabels(key) {
			switch l {
			case "S":
				srcKey = key
			case "E":
				dstKey = key
			}
		}
		return true
	})
	handle, ok := g.FirstEdgeHandle(srcKey, dstKey)
	if !ok {
		t.Fatalf("the :U relationship %q->%q carries no stable handle", srcKey, dstKey)
	}
	if handle != uint64(decoyID) {
		t.Fatalf("handle = %d, want %d (the decoy node's id): the collision this test "+
			"exists to exercise was not constructed", handle, decoyID)
	}
	return &handleCollisionFixture{eng: eng, handle: handle}
}

// assertLandedOnTheRelationship pins both halves of the defect: the write reached
// the relationship, and it reached nothing else. Asserting only the first would
// pass on an implementation that wrote to both.
func (f *handleCollisionFixture) assertLandedOnTheRelationship(t *testing.T, stmt string) {
	t.Helper()
	if got := setScalarString(t, f.eng, `MATCH ()-[rr:U]->() RETURN rr.w AS v`); got != "V" {
		t.Fatalf("rr.w = %q, want \"V\" after %s", got, stmt)
	}
	if n := outerRowCount(t, f.eng, `MATCH (d:DECOY) WHERE d.w IS NOT NULL RETURN d AS v`); n != 0 {
		t.Fatalf("%d decoy node(s) carry w after %s: the action wrote to the node whose id "+
			"equals the relationship's handle (%d)", n, stmt, f.handle)
	}
}

// TestMergeOuterRelHandleCollision_OnMatch: the ON MATCH branch of a node-only
// MERGE, for all three SET forms. Each form reaches a different writer — the
// per-property form travels [Merge.applyActions], the two whole-entity forms
// travel [Merge.applySetAllActions] — and all three resolved the target through
// the same node-first lookup.
func TestMergeOuterRelHandleCollision_OnMatch(t *testing.T) {
	t.Parallel()
	for _, form := range outerActionForms {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			f := newHandleCollisionFixture(t)
			drainRunInTx(t, f.eng, `CREATE (:P {name:'z'})`)
			stmt := `MATCH (s)-[rr:U]->(e) MERGE (z:P {name:'z'}) ON MATCH SET ` + form.render("rr")
			c := runCountedP(t, f.eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{propsSet: 1, containsUpdates: true})
			f.assertLandedOnTheRelationship(t, stmt)
		})
	}
}

// TestMergeOuterRelHandleCollision_OnCreate is the same collision on the branch
// that creates the merge node, which builds its own row before applying the
// actions and so resolves the target down a separate call path.
func TestMergeOuterRelHandleCollision_OnCreate(t *testing.T) {
	t.Parallel()
	for _, form := range outerActionForms {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			f := newHandleCollisionFixture(t)
			stmt := `MATCH (s)-[rr:U]->(e) MERGE (z:P {name:'z'}) ON CREATE SET ` + form.render("rr")
			c := runCountedP(t, f.eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{
				nodesCreated: 1, propsSet: 2, labelsAdded: 1, containsUpdates: true,
			})
			f.assertLandedOnTheRelationship(t, stmt)
		})
	}
}

// TestMergeOuterRelHandleCollision_LabelActionIsRejectedAtAnalysis records why
// the label arm of [Merge.applyActions] needs no collision guard of its own: a
// label action on a relationship never reaches the operator, because the semantic
// analyser rejects it as a type mismatch while the statement is still being
// planned. Pinned here so that, if that rejection is ever relaxed, this test
// fails and the label arm is revisited rather than quietly labelling whichever
// node happens to carry the relationship's handle.
func TestMergeOuterRelHandleCollision_LabelActionIsRejectedAtAnalysis(t *testing.T) {
	t.Parallel()
	f := newHandleCollisionFixture(t)
	drainRunInTx(t, f.eng, `CREATE (:P {name:'z'})`)
	const stmt = `MATCH (s)-[rr:U]->(e) MERGE (z:P {name:'z'}) ON MATCH SET rr:Tagged`
	err := runDrainErr(t, f.eng, stmt)
	if err == nil {
		t.Fatal("SET rr:Tagged on a relationship must be rejected, not executed")
	}
	if !strings.Contains(err.Error(), "TYPE_MISMATCH") {
		t.Fatalf("error %q should be a type mismatch", err.Error())
	}
	if n := outerRowCount(t, f.eng, `MATCH (d:Tagged) RETURN d AS v`); n != 0 {
		t.Fatalf("%d node(s) were labelled :Tagged by an action targeting a relationship", n)
	}
}
