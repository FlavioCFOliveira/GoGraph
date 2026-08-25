package lpg

// edge_prop_bool_fused_test.go — regression gate for the bit-packed-bool
// sparsify panic surfaced by the typed-schema DST scenario (rmp #2493).
//
// The defect. [edgePropColumn.grownWithValue] and
// [edgePropColumn.grownAbsentShared] — the two column-level halves of the FUSED
// build fast path [edgePropCols.GrowSlotWithValue] drives — converted a DENSE
// column to the sparse (COO) representation unconditionally. A bit-packed bool
// column has no sparse representation: [allocSparseBacking] says so in its own
// comment ("bool is never sparse"), [appendSparseValueFromDense] has no bool
// case and falls through to the boxed backing (nil for a bool column), and
// [edgePropColumn.slotValue]'s bool arm reads the bit at index i on the
// assumption that i == slot, which only holds while the column is dense.
//
// So a fused append on a source that already carried a bool edge property
// PANICKED with `index out of range [n] with length 0` inside
// [edgePropColumn.toSparse] — reachable from the public
// [Graph.AddEdgeLabeledWithProperty] with three nodes and two calls, no
// concurrency, no store and no schema. Two sibling functions already carried the
// guard ([newSparseSingleSlot] builds a bool single-slot column dense, and
// [edgePropColumn.reshaped] never demotes bool because [demoteThreshold] returns
// -1 for it), which is what made the two fused paths' omission invisible: every
// other route into the representation change was already correct.
//
// The tests below drive both halves — the bool column as the fused append's
// TARGET and as a NON-target — and assert the values afterwards, not merely the
// absence of a panic: a guard that kept the column dense but dropped its
// contents would satisfy "did not panic".

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// newBoolFusedFixture builds a directed multigraph with a source `a` already
// carrying a DENSE bool edge property on (a,b), which is the precondition both
// arms need: [Graph.SetEdgeProperty] goes through the general mutation path and
// leaves a dense, slot-indexed bit column, and the fused append is what then
// tries to sparsify it.
func newBoolFusedFixture(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %q: %v", n, err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "flag", BoolValue(true)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if v, ok := g.GetEdgeProperty("a", "b", "flag"); !ok || v.Kind() != PropBool {
		t.Fatalf("precondition: (a,b).flag is %v present=%t, want a BOOLEAN", v, ok)
	}
	return g
}

// TestFusedAppend_BoolColumnAsNonTarget drives
// [edgePropColumn.grownAbsentShared]: the fused append writes a DIFFERENT key,
// so the existing dense bool column is a non-target and is grown absent.
func TestFusedAppend_BoolColumnAsNonTarget(t *testing.T) {
	t.Parallel()
	g := newBoolFusedFixture(t)

	if err := g.AddEdgeLabeledWithProperty("a", "c", 1, "REL", "n", Int64Value(7)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
	}

	// The pre-existing bool value must be intact on its own edge...
	v, ok := g.GetEdgeProperty("a", "b", "flag")
	if !ok {
		t.Fatal("(a,b).flag disappeared after the fused append on (a,c)")
	}
	if b, isBool := v.Bool(); !isBool || !b {
		t.Fatalf("(a,b).flag = %v (bool=%t), want BOOLEAN(true)", v, isBool)
	}
	// ...and must NOT have leaked onto the new edge, which is the absent-grow's
	// whole contract.
	if got, present := g.GetEdgeProperty("a", "c", "flag"); present {
		t.Fatalf("(a,c) carries flag=%v; the non-target column's new slot must be ABSENT", got)
	}
	if got, present := g.GetEdgeProperty("a", "c", "n"); !present {
		t.Fatal("(a,c).n is absent; the fused append did not land its own value")
	} else if i, isInt := got.Int64(); !isInt || i != 7 {
		t.Fatalf("(a,c).n = %v (int=%t), want INTEGER(7)", got, isInt)
	}
}

// TestFusedAppend_BoolColumnAsTarget drives
// [edgePropColumn.grownWithValue]: the fused append writes the SAME bool key, so
// the existing dense bool column is the target and grows with a value.
func TestFusedAppend_BoolColumnAsTarget(t *testing.T) {
	t.Parallel()
	g := newBoolFusedFixture(t)

	if err := g.AddEdgeLabeledWithProperty("a", "c", 1, "REL", "flag", BoolValue(false)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
	}

	v, ok := g.GetEdgeProperty("a", "b", "flag")
	if !ok {
		t.Fatal("(a,b).flag disappeared after the fused append")
	}
	if b, isBool := v.Bool(); !isBool || !b {
		t.Fatalf("(a,b).flag = %v (bool=%t), want the original BOOLEAN(true)", v, isBool)
	}
	got, present := g.GetEdgeProperty("a", "c", "flag")
	if !present {
		t.Fatal("(a,c).flag is absent; the fused append did not land its own value")
	}
	if b, isBool := got.Bool(); !isBool || b {
		t.Fatalf("(a,c).flag = %v (bool=%t), want BOOLEAN(false)", got, isBool)
	}
}

// TestFusedAppend_BoolColumnAcrossManySlots grows the same source well past one
// slot in both roles, so the guard is exercised on a column whose validity
// bitmap spans more than the trivial case and every slot's value is checked.
//
// It is the arm that would catch a guard that kept the column dense but mixed up
// the bit offsets: a two-slot fixture cannot distinguish slot-indexed bits from
// position-indexed ones.
func TestFusedAppend_BoolColumnAcrossManySlots(t *testing.T) {
	t.Parallel()
	const targets = 40
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("src"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// One general-path (dense) bool write first, so the fused appends below all
	// meet a dense column.
	if err := g.AddNode("d0"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddEdge("src", "d0", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("src", "d0", "flag", BoolValue(true)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	want := map[string]bool{"d0": true}
	for i := 1; i <= targets; i++ {
		dst := "d" + itoaSmall(i)
		// Alternate the two roles: an even i writes the bool key (target), an odd i
		// writes an integer key (bool is the non-target).
		if i%2 == 0 {
			val := i%4 == 0
			if err := g.AddEdgeLabeledWithProperty("src", dst, 1, "REL", "flag", BoolValue(val)); err != nil {
				t.Fatalf("fused bool append %d: %v", i, err)
			}
			want[dst] = val
			continue
		}
		if err := g.AddEdgeLabeledWithProperty("src", dst, 1, "REL", "num", Int64Value(int64(i))); err != nil {
			t.Fatalf("fused int append %d: %v", i, err)
		}
	}

	for dst, expect := range want {
		v, ok := g.GetEdgeProperty("src", dst, "flag")
		if !ok {
			t.Fatalf("(src,%s).flag is absent, want BOOLEAN(%t)", dst, expect)
		}
		b, isBool := v.Bool()
		if !isBool || b != expect {
			t.Fatalf("(src,%s).flag = %v (bool=%t), want BOOLEAN(%t)", dst, v, isBool, expect)
		}
	}
	// Every odd slot wrote only `num`, so its flag must be ABSENT — the
	// absent-grow's contract, checked across the whole span rather than once.
	for i := 1; i <= targets; i += 2 {
		dst := "d" + itoaSmall(i)
		if v, present := g.GetEdgeProperty("src", dst, "flag"); present {
			t.Fatalf("(src,%s) carries flag=%v; only the even slots wrote it", dst, v)
		}
	}
}

// itoaSmall renders a small non-negative int without pulling strconv into this
// file's imports.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
