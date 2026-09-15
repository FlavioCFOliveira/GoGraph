package exec_test

// set_rel_storage_direction_test.go — operator-level regression coverage for
// rmp #2817: a relationship SET whose row carries the endpoints in TRAVERSAL
// order must write the edge at its STORAGE coordinates.
//
// The end-to-end gate lives in package cypher (set_undirected_reciprocal_test.go),
// where a real undirected MATCH produces the reversed rows. This file pins the
// same contract one layer down, at the shared binding resolver every SET form
// routes through, so a regression is attributed to the resolver rather than to
// the expansion that fed it.
//
// The fixture writes through the operator with the row's src/dst columns SWAPPED
// relative to the seeded edge — exactly the shape Expand emits for the reverse
// hop of `MATCH (a)-[r]-(b)` — and asserts the property landed on the stored
// edge, on BOTH the per-pair and the by-handle store (the #1684 congruence
// invariant).
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSetRel_ReversedRowWritesStorageDirection is the #2817 operator-level gate.
//
// It fails on the pre-fix resolver: the write was keyed ("b", "a"), where the
// seeded edge does not live, so the per-pair store of ("a", "b") kept only its
// seeded key and `stamp` was never observable on the bound relationship.
func TestSetRel_ReversedRowWritesStorageDirection(t *testing.T) {
	t.Parallel()
	const h = uint64(11)
	mut, aID, bID := newRelStub(t, h)
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"seed": lpg.Int64Value(1)})

	// The row is the REVERSE hop: src column holds b, dst column holds a, while
	// storage holds the edge as a→b. relRow's argument order is (edge, src, dst).
	op, err := exec.NewSetProperty("r", "stamp", `'written'`, map[string]int{"r": 0},
		newSliceOperator(relRow(int64(h), bID, aID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The property must be on the STORED pair (a, b) …
	pp := mut.EdgeProperties("a", "b")
	if _, ok := pp["stamp"]; !ok {
		t.Fatalf("per-pair store of the stored pair (a,b) has keys %v; the reversed row's write missed it", keysOf(pp))
	}
	// … and on the bound instance's by-handle bag, so the two stores stay
	// congruent (#1684).
	bh := mut.EdgePropertiesByHandle("a", "b", h)
	if _, ok := bh["stamp"]; !ok {
		t.Fatalf("by-handle store of handle %d has keys %v, want stamp (per-pair/by-handle congruence)", h, keysOf(bh))
	}
	// Nothing may have been planted at the non-existent reverse pair.
	if rev := mut.EdgeProperties("b", "a"); len(rev) != 0 {
		t.Fatalf("write leaked onto the reverse pair (b,a): %v", keysOf(rev))
	}
}

// TestSetRel_ForwardRowUnaffected is the control: a row already in storage order
// must behave exactly as before, so the normalisation cannot be credited with a
// result it did not produce, and no existing directed SET has moved.
func TestSetRel_ForwardRowUnaffected(t *testing.T) {
	t.Parallel()
	const h = uint64(12)
	mut, aID, bID := newRelStub(t, h)
	seedRelProps(t, mut, h, map[string]lpg.PropertyValue{"seed": lpg.Int64Value(1)})

	op, err := exec.NewSetProperty("r", "stamp", `'written'`, map[string]int{"r": 0},
		newSliceOperator(relRow(int64(h), aID, bID)), mut)
	if err != nil {
		t.Fatalf("NewSetProperty: %v", err)
	}
	op.WithRelCols(relColsAt0())
	if _, err := exec.Drain(context.Background(), op); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if _, ok := mut.EdgeProperties("a", "b")["stamp"]; !ok {
		t.Fatal("forward row: per-pair store lost the write")
	}
	if _, ok := mut.EdgePropertiesByHandle("a", "b", h)["stamp"]; !ok {
		t.Fatal("forward row: by-handle store lost the write")
	}
	if rev := mut.EdgeProperties("b", "a"); len(rev) != 0 {
		t.Fatalf("forward row leaked onto (b,a): %v", keysOf(rev))
	}
}
