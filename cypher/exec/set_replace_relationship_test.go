package exec_test

// set_replace_relationship_test.go — regression coverage for true openCypher
// REPLACE semantics of `SET r = <map|node>` on relationships (#1687).
//
// openCypher `SET r = {…}` / `SET r = node` (the `=` operator) is a REPLACE: it
// must DELETE every property on r that is ABSENT from the right-hand side. The
// mutate form `SET r += {…}` keeps absent keys. Before #1687 the relationship
// paths were overwrite-only (set each RHS key, never cleared the others), so a
// rel with {a:1, b:2} then `SET r = {a:9}` wrongly kept b:2. The TCK does not
// cover the distinct-extra-key removal case for relationships (Merge7 [4]/[5]
// use a single key; Set4 map-replace is node-only), hence these impl-extension
// tests.
//
// Each test drives the write operator over a single relationship row using the
// stubMutator (real per-pair AND by-handle property stores) and asserts the
// post-state of BOTH stores, so the by-handle mirror stays congruent with the
// per-pair store (the #1684 invariant).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nodeIDValue encodes a NodeID as the IntegerValue a node column carries in the
// pipeline (matching what Expand / ScanAll emit). nodeIDValueU is the uint64
// convenience form used by the MergeRelationship endpoint columns.
func nodeIDValue(id graph.NodeID) expr.Value { return expr.IntegerValue(int64(id)) }
func nodeIDValueU(id uint64) expr.Value      { return expr.IntegerValue(int64(id)) }

// seedRelProps writes key→value on both the per-pair and the by-handle store of
// edge (a, b) at the given handle, modelling a relationship created earlier with
// those properties.
func seedRelProps(t *testing.T, mut *stubMutator, handle uint64, props map[string]lpg.PropertyValue) {
	t.Helper()
	for k, v := range props {
		if err := mut.SetEdgeProperty("a", "b", k, v); err != nil {
			t.Fatalf("seed per-pair %q: %v", k, err)
		}
		if handle != 0 {
			if err := mut.SetEdgePropertyByHandle("a", "b", handle, k, v); err != nil {
				t.Fatalf("seed by-handle %q: %v", k, err)
			}
		}
	}
}

// assertEdgeKeys asserts that the per-pair store of (a, b) has exactly wantKeys
// (and, when handle != 0, that the by-handle store matches it key-for-key).
func assertEdgeKeys(t *testing.T, mut *stubMutator, handle uint64, wantKeys ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(wantKeys))
	for _, k := range wantKeys {
		want[k] = struct{}{}
	}
	pp := mut.EdgeProperties("a", "b")
	if len(pp) != len(want) {
		t.Fatalf("per-pair keys = %v, want %v", keysOf(pp), wantKeys)
	}
	for k := range pp {
		if _, ok := want[k]; !ok {
			t.Fatalf("per-pair has unexpected key %q (want %v)", k, wantKeys)
		}
	}
	if handle == 0 {
		return
	}
	bh := mut.EdgePropertiesByHandle("a", "b", handle)
	if len(bh) != len(want) {
		t.Fatalf("by-handle keys = %v, want %v (congruence with per-pair, #1684)", keysOf(bh), wantKeys)
	}
	for k := range bh {
		if _, ok := want[k]; !ok {
			t.Fatalf("by-handle has unexpected key %q (want %v)", k, wantKeys)
		}
	}
}

func keysOf(m map[string]lpg.PropertyValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustInt(t *testing.T, m map[string]lpg.PropertyValue, key string, want int64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing from %v", key, keysOf(m))
	}
	got, ok := v.Int64()
	if !ok {
		t.Fatalf("key %q is not an integer", key)
	}
	if got != want {
		t.Fatalf("key %q = %d, want %d", key, got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SetProperty whole-entity replace on a relationship (set.go applyToRelationship)
// ─────────────────────────────────────────────────────────────────────────────

// TestSetRel_WholeEntityReplace_DropsAbsentKeys is the core #1687 regression for
// the SetProperty whole-entity path: a rel with {a:1, b:2} then `SET r = {a:9}`
// must end up with {a:9} only — b dropped — on BOTH stores.
func TestSetRel_WholeEntityReplace_DropsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(7)
	mut, aID, bID := newRelStub(t, h)
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})

	op, err := exec.NewSetProperty("r", "", `{a: 9}`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "a")
	mustInt(t, mut.EdgeProperties("a", "b"), "a", 9)
	mustInt(t, mut.EdgePropertiesByHandle("a", "b", h), "a", 9)
}

// TestSetRel_Merge_KeepsAbsentKeys confirms `SET r += {a:9}` is still additive:
// it overwrites a, keeps b. The mutate form must NOT clear.
func TestSetRel_Merge_KeepsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(8)
	mut, aID, bID := newRelStub(t, h)
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})

	op, err := exec.NewSetProperty("r", "", `+= {a: 9}`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty(+=): %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "a", "b")
	mustInt(t, mut.EdgeProperties("a", "b"), "a", 9)
	mustInt(t, mut.EdgeProperties("a", "b"), "b", 2)
}

// TestSetRel_WholeEntityReplace_NoHandleClearsPerPairOnly verifies the replace
// also clears absent keys when no stable handle is resolvable (per-pair-only
// fallback) without touching the by-handle store.
func TestSetRel_WholeEntityReplace_NoHandleClearsPerPairOnly(t *testing.T) {
	t.Parallel()
	mut, aID, bID := newRelStub(t, 0) // no handle
	// Seed per-pair only (by-handle store is keyed by a real handle).
	if err := mut.SetEdgeProperty("a", "b", "a", lpg.Int64Value(1)); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := mut.SetEdgeProperty("a", "b", "b", lpg.Int64Value(2)); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	op, err := exec.NewSetProperty("r", "", `{a: 9}`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, 0, "a")
}

// TestSetRel_ReplaceToEmpty drops every property: `SET r = {}` clears all.
func TestSetRel_ReplaceToEmpty(t *testing.T) {
	t.Parallel()
	const h = uint64(15)
	mut, aID, bID := newRelStub(t, h)
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})

	op, err := exec.NewSetProperty("r", "", `{}`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h)
}

// TestSetNode_WholeEntityReplace_Unchanged guards that node REPLACE behaviour is
// unchanged by the #1687 relationship fix: a node with {a:1, b:2} then
// `SET n = {a:9}` keeps only a (the node path already cleared).
func TestSetNode_WholeEntityReplace_Unchanged(t *testing.T) {
	t.Parallel()
	mut := newStubMutator()
	nID := mustAddNode(t, mut, "n")
	if err := mut.SetNodeProperty("n", "a", lpg.Int64Value(1)); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := mut.SetNodeProperty("n", "b", lpg.Int64Value(2)); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	op, err := exec.NewSetProperty("n", "", `{a: 9}`, map[string]int{"n": 0}, newSliceOperator(exec.Row{nodeIDValue(nID)}), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	props := mut.NodeProperties("n")
	if len(props) != 1 {
		t.Fatalf("node props = %v, want only {a}", keysOf(props))
	}
	mustInt(t, props, "a", 9)
}

// ─────────────────────────────────────────────────────────────────────────────
// MergeRelationship ON MATCH / ON CREATE  SET r = {…} / node  (#1687)
// ─────────────────────────────────────────────────────────────────────────────

// newMergeRelStub seeds two nodes, wires a firstHandle resolver returning the
// supplied handle, and returns the mutator plus the endpoint NodeIDs.
func newMergeRelStub(t *testing.T, handle uint64) (*stubMutator, uint64, uint64) {
	t.Helper()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mut.firstHandle = func(_, _ string) (uint64, bool) {
		if handle == 0 {
			return 0, false
		}
		return handle, true
	}
	return mut, uint64(aID), uint64(bID)
}

// mergeRow lays out (srcID, dstID) at columns 0 and 1 for MergeRelationship.
func mergeRow(srcID, dstID uint64) exec.Row {
	return exec.Row{nodeIDValueU(srcID), nodeIDValueU(dstID)}
}

// TestMergeRel_OnMatchReplaceMap_DropsAbsentKeys is the user-visible #1687 bug:
// a matched edge with {a:1, b:2} under `ON MATCH SET r = {a:9}` must end with
// {a:9} only on both stores.
func TestMergeRel_OnMatchReplaceMap_DropsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(21)
	mut, srcID, dstID := newMergeRelStub(t, h)
	// Pre-existing typed edge with two properties.
	mustAddEdge(t, mut, "a", "b", 0)
	mut.SetEdgeLabel("a", "b", "T")
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})

	op := exec.NewMergeRelationship(newSliceOperator(mergeRow(srcID, dstID)), 0, 1, "T", mut).
		WithSchema(map[string]int{"r": 2})
	op = op.WithOnMatch("r", []exec.MergeRelAction{
		exec.MergeRelActionReplaceFromKV("", "", true, []string{"a"}),
		exec.MergeRelActionReplaceFromKV("a", "9", false, nil),
	})
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "a")
	mustInt(t, mut.EdgeProperties("a", "b"), "a", 9)
	mustInt(t, mut.EdgePropertiesByHandle("a", "b", h), "a", 9)
}

// TestMergeRel_OnMatchMergeMap_KeepsAbsentKeys confirms the `+=` form inside
// MERGE stays additive (no sentinel emitted → no clear).
func TestMergeRel_OnMatchMergeMap_KeepsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(22)
	mut, srcID, dstID := newMergeRelStub(t, h)
	mustAddEdge(t, mut, "a", "b", 0)
	mut.SetEdgeLabel("a", "b", "T")
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})

	op := exec.NewMergeRelationship(newSliceOperator(mergeRow(srcID, dstID)), 0, 1, "T", mut).
		WithSchema(map[string]int{"r": 2})
	// `+= {a:9}` decomposes to a single write action, replace=false, no sentinel.
	op = op.WithOnMatch("r", []exec.MergeRelAction{
		exec.MergeRelActionReplaceFromKV("a", "9", false, nil),
	})
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "a", "b")
	mustInt(t, mut.EdgeProperties("a", "b"), "a", 9)
	mustInt(t, mut.EdgeProperties("a", "b"), "b", 2)
}

// TestMergeRel_OnMatchReplaceEntityCopy_DropsAbsentKeys covers
// `ON MATCH SET r = node`: the matched edge {a:1, b:2} must end with exactly the
// source node's property set ({x:5}), dropping a and b.
func TestMergeRel_OnMatchReplaceEntityCopy_DropsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(23)
	mut, srcID, dstID := newMergeRelStub(t, h)
	mustAddEdge(t, mut, "a", "b", 0)
	mut.SetEdgeLabel("a", "b", "T")
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})
	// Source node `n` carries a single property x:5.
	nID := mustAddNode(t, mut, "n")
	if err := mut.SetNodeProperty("n", "x", lpg.Int64Value(5)); err != nil {
		t.Fatalf("seed source node: %v", err)
	}

	op := exec.NewMergeRelationship(newSliceOperator(exec.Row{nodeIDValueU(srcID), nodeIDValueU(dstID), nodeIDValue(nID)}), 0, 1, "T", mut).
		WithSchema(map[string]int{"r": 3, "n": 2})
	// Entity-copy replace: key="" value="n" replace=true.
	op = op.WithOnMatch("r", []exec.MergeRelAction{
		exec.MergeRelActionReplaceFromKV("", "n", true, nil),
	})
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "x")
	mustInt(t, mut.EdgeProperties("a", "b"), "x", 5)
	mustInt(t, mut.EdgePropertiesByHandle("a", "b", h), "x", 5)
}

// TestMergeRel_OnMatchMergeEntityCopy_KeepsAbsentKeys confirms `SET r += node`
// (entity-copy mutate) keeps the edge's pre-existing keys.
func TestMergeRel_OnMatchMergeEntityCopy_KeepsAbsentKeys(t *testing.T) {
	t.Parallel()
	const h = uint64(24)
	mut, srcID, dstID := newMergeRelStub(t, h)
	mustAddEdge(t, mut, "a", "b", 0)
	mut.SetEdgeLabel("a", "b", "T")
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"a": lpg.Int64Value(1), "b": lpg.Int64Value(2)})
	nID := mustAddNode(t, mut, "n")
	if err := mut.SetNodeProperty("n", "x", lpg.Int64Value(5)); err != nil {
		t.Fatalf("seed source node: %v", err)
	}

	op := exec.NewMergeRelationship(newSliceOperator(exec.Row{nodeIDValueU(srcID), nodeIDValueU(dstID), nodeIDValue(nID)}), 0, 1, "T", mut).
		WithSchema(map[string]int{"r": 3, "n": 2})
	op = op.WithOnMatch("r", []exec.MergeRelAction{
		exec.MergeRelActionReplaceFromKV("", "n", false, nil), // += node
	})
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, h, "a", "b", "x")
}

// TestMergeRel_OnCreateReplaceMap_OnFreshEdge exercises the ON CREATE path: a
// brand-new edge gets `SET r = {a:9}`. There are no pre-existing properties to
// clear, so the result is simply {a:9} — but this drives the clear branch on a
// freshly-allocated handle (clearRelPropsAbsent sees an empty store and no-ops).
func TestMergeRel_OnCreateReplaceMap_OnFreshEdge(t *testing.T) {
	t.Parallel()
	mut, srcID, dstID := newMergeRelStub(t, 0) // no firstHandle; AddEdgeH mints one
	op := exec.NewMergeRelationship(newSliceOperator(mergeRow(srcID, dstID)), 0, 1, "T", mut).
		WithSchema(map[string]int{"r": 2})
	op = op.WithOnCreate("r", []exec.MergeRelAction{
		exec.MergeRelActionReplaceFromKV("", "", true, []string{"a"}),
		exec.MergeRelActionReplaceFromKV("a", "9", false, nil),
	})
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	assertEdgeKeys(t, mut, 0, "a")
	mustInt(t, mut.EdgeProperties("a", "b"), "a", 9)
}
