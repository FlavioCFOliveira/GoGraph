package exec_test

// byhandle_dualwrite_test.go — operator-level coverage for the by-handle
// edge-property dual-write that SetProperty / SetAllProperties / RemoveProperty
// perform for a bound relationship (#1686).
//
// Each test drives the write operator over a single relationship row using the
// stubMutator (whose by-handle property store and EdgeHandleAtPosition resolver
// are programmable), and asserts that the per-pair store AND the per-instance
// by-handle store are both updated for the bound instance — and ONLY for the
// resolved handle. When the resolver returns 0 (no stable handle) the by-handle
// store must be left untouched (per-pair-only fallback).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relRow builds the (edgePos, srcID, dstID) row layout the Expand operator emits
// for a bound relationship variable, with the rel column at schema index 0.
func relRow(edgePos int64, srcID, dstID uint64) exec.Row {
	return exec.Row{
		expr.IntegerValue(edgePos),
		expr.IntegerValue(int64(srcID)),
		expr.IntegerValue(int64(dstID)),
	}
}

// relColsAt0 is the RelCols for a rel variable at schema column 0 with src/dst
// at 1/2 and the edge-position counter at column 0 (EdgeCol).
func relColsAt0() exec.RelCols {
	return exec.RelCols{SrcCol: 1, DstCol: 2, EdgeCol: 0}
}

// newRelStub seeds two nodes and a per-pair edge, and wires a handle resolver
// that maps the bound edge position to the supplied handle.
func newRelStub(t *testing.T, handle uint64) (*stubMutator, uint64, uint64) {
	t.Helper()
	mut := newStubMutator()
	aID := mustAddNode(t, mut, "a")
	bID := mustAddNode(t, mut, "b")
	mustAddEdge(t, mut, "a", "b", 0)
	mut.handleAt = func(_, _ string, _ uint64) uint64 { return handle }
	return mut, uint64(aID), uint64(bID)
}

func TestByHandleDualWrite_SetSingleProperty(t *testing.T) {
	t.Parallel()
	const h = uint64(7)
	mut, aID, bID := newRelStub(t, h)

	op, err := exec.NewSetProperty("r", "tag", `"v1"`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Per-pair store updated (authoritative for reads).
	if _, ok := mut.EdgeProperties("a", "b")["tag"]; !ok {
		t.Fatalf("per-pair tag not set")
	}
	// By-handle store updated for the resolved handle only.
	bh := mut.EdgePropertiesByHandle("a", "b", h)
	if v, ok := bh["tag"]; !ok {
		t.Fatalf("by-handle tag not set on handle %d: %v", h, bh)
	} else if s, _ := v.String(); s != "v1" {
		t.Fatalf("by-handle tag = %q, want v1", s)
	}
}

func TestByHandleDualWrite_SetMerge(t *testing.T) {
	t.Parallel()
	const h = uint64(9)
	mut, aID, bID := newRelStub(t, h)

	op, err := exec.NewSetProperty("r", "", `+= {a: 1, b: 2}`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty(+=): %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	bh := mut.EdgePropertiesByHandle("a", "b", h)
	for _, k := range []string{"a", "b"} {
		if _, ok := bh[k]; !ok {
			t.Fatalf("by-handle merge: key %q missing: %v", k, bh)
		}
	}
}

func TestByHandleDualWrite_SetNullDeletesByHandle(t *testing.T) {
	t.Parallel()
	const h = uint64(11)
	mut, aID, bID := newRelStub(t, h)
	// Pre-seed the by-handle store with the key, then SET r.tag = null.
	if err := mut.SetEdgePropertyByHandle("a", "b", h, "tag", lpg.StringValue("seed")); err != nil {
		t.Fatalf("seed by-handle: %v", err)
	}
	if err := mut.SetEdgeProperty("a", "b", "tag", lpg.StringValue("seed")); err != nil {
		t.Fatalf("seed per-pair: %v", err)
	}

	op, err := exec.NewSetProperty("r", "tag", `null`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty(null): %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if bh := mut.EdgePropertiesByHandle("a", "b", h); len(bh) != 0 {
		t.Fatalf("SET r.tag = null must remove the by-handle key, got %v", bh)
	}
	if pp := mut.EdgeProperties("a", "b"); len(pp) != 0 {
		t.Fatalf("SET r.tag = null must remove the per-pair key, got %v", pp)
	}
}

func TestByHandleDualWrite_Remove(t *testing.T) {
	t.Parallel()
	const h = uint64(13)
	mut, aID, bID := newRelStub(t, h)
	if err := mut.SetEdgePropertyByHandle("a", "b", h, "tag", lpg.StringValue("seed")); err != nil {
		t.Fatalf("seed by-handle: %v", err)
	}
	if err := mut.SetEdgeProperty("a", "b", "tag", lpg.StringValue("seed")); err != nil {
		t.Fatalf("seed per-pair: %v", err)
	}

	op := exec.NewRemoveProperty("r", "tag", map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if bh := mut.EdgePropertiesByHandle("a", "b", h); len(bh) != 0 {
		t.Fatalf("REMOVE r.tag must remove the by-handle key, got %v", bh)
	}
}

// TestByHandleDualWrite_NoHandleFallsBackToPerPairOnly verifies that when the
// resolver returns 0 (no stable handle, e.g. simple-graph storage), the
// by-handle store is left completely untouched and only the per-pair store is
// written — the mutation never lands on a wrong instance.
func TestByHandleDualWrite_NoHandleFallsBackToPerPairOnly(t *testing.T) {
	t.Parallel()
	mut, aID, bID := newRelStub(t, 0) // resolver returns 0

	op, err := exec.NewSetProperty("r", "tag", `"v1"`, map[string]int{"r": 0}, newSliceOperator(relRow(0, aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if _, ok := mut.EdgeProperties("a", "b")["tag"]; !ok {
		t.Fatalf("per-pair tag must still be set in the no-handle fallback")
	}
	// No by-handle entry should exist for any handle (the store stays empty).
	if bh := mut.EdgePropertiesByHandle("a", "b", 1); len(bh) != 0 {
		t.Fatalf("no-handle fallback must not touch the by-handle store, got %v", bh)
	}
}
