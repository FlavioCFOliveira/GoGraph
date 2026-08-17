package sim

// merge_outer_rel_handle_test.go — the MERGE-surface arm for rmp #2515: a
// node-only MERGE whose ON CREATE / ON MATCH action targets a RELATIONSHIP bound
// by a preceding clause, driven in the one graph state that makes the defect
// observable.
//
// # Why this arm is scripted rather than driven by an actor
//
// [tmplMergePairOuterRel] already drives an outer-relationship action, but it
// routes to the whole-pattern MergePattern operator, which resolves an action
// target by VARIABLE NAME against its own chain and so was never exposed. The
// node-only Merge operator resolves it by reading the target's row column as a
// node id — and since rmp #2317 a relationship's column holds its stable HANDLE,
// which shares its representation with [graph.NodeID]. The write therefore landed
// on whatever unrelated node happened to carry the handle's value, and only on a
// graph that had one; every other graph wrote to the right entity.
//
// That precondition is a property of the graph, not of the statement, so no
// actor draw can be relied on to produce it: the equivalent end-to-end matrix in
// cypher/merge_outer_target_test.go reproduced it on about 1% of its runs. Here
// the collision is CONSTRUCTED — [lpg.Graph.SeedEdgeHandle] raises the per-graph
// handle counter so the next relationship the engine creates is stamped with the
// id of a Person created beforehand — and then VERIFIED against
// [lpg.Graph.FirstEdgeHandle] before the statement runs, so the arm cannot decay
// into passing for the wrong reason.
//
// The graph is configured exactly as the simulator's default engine is
// (directed, non-multigraph), so the state pinned here is one an ordinary run can
// reach.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// tmplMergeNodeOuterRel is the statement this arm pins: a NODE-only MERGE whose
// branch action writes to the relationship the preceding MATCH bound. It is a
// scripted constant rather than a workload template — the collision it needs is a
// graph precondition an actor cannot be relied on to produce, so registering it
// with the actors and the non-vacuity gate would buy coverage that only sometimes
// adjudicates anything.
const tmplMergeNodeOuterRel = "MATCH (x:Person {name:$x})-[k:PAIRED]->(y:Person {name:$y}) " +
	"MERGE (n:Person {name:$n}) %s SET k." + mergePairRelKey + " = $v"

// outerRelCollisionFixture is an engine whose single PAIRED relationship carries
// a stable handle equal to the id of one of its Person nodes.
type outerRelCollisionFixture struct {
	adapter *EngineAdapter
	handle  uint64
}

// newOuterRelCollisionFixture builds that engine: two named Persons, the handle
// counter seeded to the larger of their ids, and only then the PAIRED edge — so
// the engine's own write path stamps the colliding handle.
func newOuterRelCollisionFixture(t *testing.T) *outerRelCollisionFixture {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	a := NewEngineAdapter(cypher.NewEngine(g))

	for _, name := range []string{"wp0", "wp1"} {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": name, "age": int64(1)}}
		if committed, _ := runCountedOp(t, a, op); !committed {
			t.Fatalf("seed CREATE %q did not commit", name)
		}
	}

	// Node ids start at 0 and 0 is the reserved "no handle" sentinel, so the
	// larger of the two ids is the one that can be a handle.
	var decoyID graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if id > decoyID {
			decoyID = id
		}
		return true
	})
	if decoyID == 0 {
		t.Fatal("no seeded Person received a non-zero id; the collision cannot be constructed")
	}
	g.SeedEdgeHandle(uint64(decoyID))

	edge := Op{Kind: OpCreate, Cypher: "MATCH (x:Person {name:$x}),(y:Person {name:$y}) CREATE (x)-[:PAIRED]->(y)",
		Params: map[string]any{"x": "wp0", "y": "wp1"}}
	if committed, _ := runCountedOp(t, a, edge); !committed {
		t.Fatal("seed CREATE of the PAIRED edge did not commit")
	}

	var srcKey, dstKey string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		name, ok := g.NodeProperties(key)["name"]
		if !ok {
			return true
		}
		switch s, isStr := name.String(); {
		case !isStr:
		case s == "wp0":
			srcKey = key
		case s == "wp1":
			dstKey = key
		}
		return true
	})
	handle, ok := g.FirstEdgeHandle(srcKey, dstKey)
	if !ok {
		t.Fatalf("the PAIRED edge %q->%q carries no stable handle", srcKey, dstKey)
	}
	if handle != uint64(decoyID) {
		t.Fatalf("handle = %d, want %d (a Person's node id): the collision this arm exists "+
			"to exercise was not constructed", handle, decoyID)
	}
	return &outerRelCollisionFixture{adapter: a, handle: handle}
}

// assertLandedOnTheRelationship pins both halves: the action reached the
// relationship, and it reached no node. Either alone would pass an engine that
// wrote to both.
func (f *outerRelCollisionFixture) assertLandedOnTheRelationship(t *testing.T, want string) {
	t.Helper()
	got, err := f.adapter.projectRowStrings(context.Background(),
		"MATCH (x:Person {name:'wp0'})-[k:PAIRED]->(y:Person {name:'wp1'}) RETURN k."+mergePairRelKey, 1)
	if err != nil {
		t.Fatalf("relationship read-back: %v", err)
	}
	if got[0] != want {
		t.Fatalf("k.%s = %s, want %s: the outer-relationship action did not reach the relationship",
			mergePairRelKey, got[0], want)
	}
	n, err := f.adapter.scalarCount(
		"MATCH (p:Person) WHERE p." + mergePairRelKey + " IS NOT NULL RETURN count(p)")
	if err != nil {
		t.Fatalf("node probe: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d node(s) carry %s: the action wrote to the node whose id equals the "+
			"relationship's stable handle (%d)", n, mergePairRelKey, f.handle)
	}
}

// TestMergeSurface_OuterRelHandleCollision_OnMatch drives the ON MATCH branch:
// the merged Person already exists, so the only effect the statement may report
// is the one property the outer-relationship action writes.
func TestMergeSurface_OuterRelHandleCollision_OnMatch(t *testing.T) {
	f := newOuterRelCollisionFixture(t)
	op := mergeOp(fmtMergeNodeOuterRel("ON MATCH"), map[string]any{
		"x": "wp0", "y": "wp1", "n": "wp0", "v": int64(17)})
	committed, counters := runCountedOp(t, f.adapter, op)
	if !committed {
		t.Fatal("node MERGE with an outer-relationship ON MATCH action did not commit")
	}
	wantCounters(t, "node MERGE / ON MATCH outer relationship", counters,
		&exec.QueryCounters{PropertiesSet: 1})
	f.assertLandedOnTheRelationship(t, "17")
}

// TestMergeSurface_OuterRelHandleCollision_OnCreate drives the ON CREATE branch,
// which builds its own row before applying the actions and so resolves the target
// down a separate path: the merged Person is a name no seed used.
func TestMergeSurface_OuterRelHandleCollision_OnCreate(t *testing.T) {
	f := newOuterRelCollisionFixture(t)
	op := mergeOp(fmtMergeNodeOuterRel("ON CREATE"), map[string]any{
		"x": "wp0", "y": "wp1", "n": "wp2", "v": int64(19)})
	committed, counters := runCountedOp(t, f.adapter, op)
	if !committed {
		t.Fatal("node MERGE with an outer-relationship ON CREATE action did not commit")
	}
	// The merged node's name plus the relationship property.
	wantCounters(t, "node MERGE / ON CREATE outer relationship", counters,
		&exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2})
	f.assertLandedOnTheRelationship(t, "19")
}

// fmtMergeNodeOuterRel renders [tmplMergeNodeOuterRel] for one branch keyword.
func fmtMergeNodeOuterRel(branch string) string {
	return fmt.Sprintf(tmplMergeNodeOuterRel, branch)
}
