package lpg

// mvcc_props_test.go — MVCC P2 (rmp #2279): node properties on delta chains.
//
// Layer: short.
//
// Properties are the first structure whose undo record carries a VALUE, so
// three things need pinning that labels did not need: that all three
// transitions reconstruct (absent to value, value to value, value to absent),
// that the size the cost model rests on is a constant, and that a value whose
// payload is UNCOMPARABLE does not crash the write path.

import (
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func propGraph(t *testing.T, nodes ...string) (*Graph[string, float64], map[string]graph.NodeID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	ids := make(map[string]graph.NodeID, len(nodes))
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
		id, ok := g.adj.Mapper().Lookup(n)
		if !ok {
			t.Fatalf("%s not interned", n)
		}
		ids[n] = id
	}
	g.EnablePropDeltas()
	return g, ids
}

// TestPropDelta_AllThreeTransitionsReconstruct is the correctness core: each of
// the three ways a property can change must be reversible from its undo record.
func TestPropDelta_AllThreeTransitionsReconstruct(t *testing.T) {
	g, ids := propGraph(t, "a")
	id := ids["a"]
	keyID := g.propKeys().Intern("w")

	t.Run("absent to value", func(t *testing.T) {
		before := g.readTS()
		if err := g.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		old := g.propBagAsOf(id, before, 0)
		if _, had := old.get(keyID); had {
			t.Fatal("a reader from before the write can see the new key")
		}
		now := g.propBagAsOf(id, g.readTS(), 0)
		if v, had := now.get(keyID); !had || v.v != int64(1) {
			t.Fatalf("a reader from after the write sees %v/%v, want 1", v.v, had)
		}
	})

	t.Run("value to value", func(t *testing.T) {
		before := g.readTS()
		if err := g.SetNodeProperty("a", "w", Int64Value(2)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		old := g.propBagAsOf(id, before, 0)
		if v, _ := old.get(keyID); v.v != int64(1) {
			t.Fatalf("the older version reads %v, want the pre-image 1", v.v)
		}
		now := g.propBagAsOf(id, g.readTS(), 0)
		if v, _ := now.get(keyID); v.v != int64(2) {
			t.Fatalf("the current version reads %v, want 2", v.v)
		}
	})

	t.Run("value to absent", func(t *testing.T) {
		before := g.readTS()
		g.DelNodeProperty("a", "w")
		old := g.propBagAsOf(id, before, 0)
		if v, had := old.get(keyID); !had || v.v != int64(2) {
			t.Fatalf("the older version lost the deleted key (%v/%v), want 2", v.v, had)
		}
		now := g.propBagAsOf(id, g.readTS(), 0)
		if _, had := now.get(keyID); had {
			t.Fatal("the current version still carries the deleted key")
		}
	})
}

// TestPropDelta_UncomparableValueDoesNotPanic pins the crash the obvious
// implementation would have shipped.
//
// PropertyValue carries its payload in an `any`, and PropBytes and PropList
// hold slices. Deciding "did this write change anything?" with `prev != value`
// panics with "comparing uncomparable type" the first time a byte or list
// property is overwritten — on an ordinary write path, reachable from any SET
// of a list-valued property.
func TestPropDelta_UncomparableValueDoesNotPanic(t *testing.T) {
	g, _ := propGraph(t, "a")
	for _, v := range [][2]PropertyValue{
		{BytesValue([]byte{1, 2, 3}), BytesValue([]byte{4, 5, 6})},
		{ListValue([]PropertyValue{Int64Value(1)}), ListValue([]PropertyValue{Int64Value(2)})},
	} {
		if err := g.SetNodeProperty("a", "k", v[0]); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if err := g.SetNodeProperty("a", "k", v[1]); err != nil {
			t.Fatalf("overwrite of an uncomparable value: %v", err)
		}
		g.DelNodeProperty("a", "k")
	}
}

// TestPropDelta_NoDeltaForARedundantWrite pins that re-writing an identical
// scalar value records nothing, so a workload that re-asserts properties does
// not grow the chain without bound.
func TestPropDelta_NoDeltaForARedundantWrite(t *testing.T) {
	g, _ := propGraph(t, "a")
	for i := 0; i < 10; i++ {
		if err := g.SetNodeProperty("a", "w", Int64Value(7)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if n := g.PropDeltaCount(); n != 1 {
		t.Fatalf("ten identical writes produced %d deltas, want 1", n)
	}
	// An uncomparable value is treated conservatively — it MAY record a delta
	// it did not need to, which is the safe direction, and is asserted here so
	// the conservatism is deliberate rather than accidental.
	if err := g.SetNodeProperty("a", "b", BytesValue([]byte{1})); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	n0 := g.PropDeltaCount()
	if err := g.SetNodeProperty("a", "b", BytesValue([]byte{1})); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if g.PropDeltaCount() != n0+1 {
		t.Fatalf("an identical UNCOMPARABLE write should conservatively record a delta; "+
			"count went %d -> %d", n0, g.PropDeltaCount())
	}
}

// TestPropTx_CommitIsAtomic mirrors the label case: a transaction's property
// writes become visible all at once, and are invisible before that.
func TestPropTx_CommitIsAtomic(t *testing.T) {
	g, ids := propGraph(t, "a", "b")
	keyID := g.propKeys().Intern("w")
	before := g.readTS()

	tx := g.beginLabelTx()
	for n := range ids {
		if err := tx.setNodeProperty(n, "w", Int64Value(9)); err != nil {
			t.Fatalf("setNodeProperty: %v", err)
		}
	}
	for n, id := range ids {
		if bag := g.propBagAsOf(id, before, 0); func() bool { _, had := bag.get(keyID); return had }() {
			t.Fatalf("node %s shows an uncommitted property to another reader", n)
		}
		if bag := tx.propsOf(id); func() bool { _, had := bag.get(keyID); return !had }() {
			t.Fatalf("node %s: the writing transaction cannot see its own write", n)
		}
	}
	commitTS := mustCommit(t, tx)
	for n, id := range ids {
		if bag := g.propBagAsOf(id, commitTS, 0); func() bool { _, had := bag.get(keyID); return !had }() {
			t.Fatalf("node %s: a reader after the commit cannot see the property", n)
		}
		if bag := g.propBagAsOf(id, before, 0); func() bool { _, had := bag.get(keyID); return had }() {
			t.Fatalf("node %s: a reader from before the commit can see it", n)
		}
	}
	// Exercise the transactional delete, and confirm it too is atomic.
	tx2 := g.beginLabelTx()
	for n := range ids {
		tx2.delNodeProperty(n, "w")
	}
	for n, id := range ids {
		if bag := g.propBagAsOf(id, commitTS, 0); func() bool { _, had := bag.get(keyID); return !had }() {
			t.Fatalf("node %s lost the property to an UNCOMMITTED delete", n)
		}
	}
	after := mustCommit(t, tx2)
	for n, id := range ids {
		if bag := g.propBagAsOf(id, after, 0); func() bool { _, had := bag.get(keyID); return had }() {
			t.Fatalf("node %s still carries the property after the delete committed", n)
		}
	}
}

// TestNodePropDelta_SizeIsPinned guards the cost model, as the label one does.
func TestNodePropDelta_SizeIsPinned(t *testing.T) {
	const want = 56
	if got := unsafe.Sizeof(nodePropDelta{}); got != want {
		t.Fatalf("nodePropDelta is %d bytes, want %d — the per-modification memory cost is the "+
			"cost model this programme was authorised on, so a change needs a re-measurement "+
			"rather than a new constant", got, want)
	}
}

// TestPropDelta_ArmedByDefaultAndDisarmable pins both halves of the P4a
// contract change (rmp #2288): property versioning is on out of the box, and
// [Graph.DisableMVCC] returns it to recording nothing. It replaces
// TestPropDelta_DisabledByDefault, whose assertion was the deliberate opposite.
func TestPropDelta_ArmedByDefaultAndDisarmable(t *testing.T) {
	armed := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := armed.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := armed.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	armed.DelNodeProperty("a", "w")
	if n := armed.PropDeltaCount(); n != 2 {
		t.Fatalf("a default graph recorded %d property deltas for one set and one delete, want 2", n)
	}

	inert := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	inert.disarmMVCCForTest()
	if err := inert.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := inert.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	inert.DelNodeProperty("a", "w")
	if n := inert.PropDeltaCount(); n != 0 {
		t.Fatalf("a disarmed graph recorded %d property deltas; DisableMVCC must record nothing", n)
	}
}
