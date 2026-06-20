package lpg

// edge_has_property_test.go — sprint 222 #1638. Unit coverage for
// Graph.EdgeHasProperty, the kind-gated storage presence check behind a bound
// relationship's `r.k IS [NOT] NULL` predicate. The contract under test:
// EdgeHasProperty(src,dst,k) is true iff the per-pair COALESCED value for k
// would materialise to a NON-NULL Cypher value — i.e. present AND the winning
// slot's kind maps to a non-null value via the same table cypher.lpgPropToExpr
// uses. It must also be allocation-free and read only validity bits + kind
// tags, never the value cell. Layer: short.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// timeFixture is a fixed PropTime value used to exercise the null-mapping kind
// gate (a stored time.Time reads back as Null through Cypher).
func timeFixture() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }

// dateTagged builds the SOH-Date-tagged canonical-text string that the columnar
// tier folds into the internal dateKind column (and that cypher.lpgPropToExpr
// decodes back to a non-null Cypher Date on read).
func dateTagged(body string) string { return string(rune(epochDayTag)) + body }

func TestGraph_EdgeHasProperty_PresenceAndKindGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value PropertyValue
		want  bool // expected EdgeHasProperty — true iff non-null-mapping
	}{
		{"string", StringValue("hi"), true},
		{"int64", Int64Value(7), true},
		{"float64", Float64Value(1.5), true},
		{"bool_false", BoolValue(false), true}, // present-but-false is NON-null
		{"bool_true", BoolValue(true), true},
		{"int_zero", Int64Value(0), true}, // 0 is a legal value, not absence
		{"list", ListValue([]PropertyValue{Int64Value(1)}), true},
		{"date", StringValue(dateTagged("2020-01-01")), true}, // dateKind -> non-null Date
		{"time", TimeValue(timeFixture()), false},             // PropTime -> Null
		{"bytes", BytesValue([]byte{1, 2, 3}), false},         // PropBytes -> Null
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New[string, int64](adjlist.Config{Directed: true})
			if err := g.AddEdge("a", "b", 0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if err := g.SetEdgeProperty("a", "b", "k", tc.value); err != nil {
				t.Fatalf("SetEdgeProperty: %v", err)
			}
			if got := g.EdgeHasProperty("a", "b", "k"); got != tc.want {
				t.Fatalf("EdgeHasProperty kind %v = %v, want %v", tc.value.Kind(), got, tc.want)
			}
			// A different key is always absent.
			if g.EdgeHasProperty("a", "b", "other") {
				t.Fatalf("EdgeHasProperty on absent key must be false")
			}
		})
	}
}

func TestGraph_EdgeHasProperty_Absent(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// Edge exists but carries no properties at all.
	if g.EdgeHasProperty("a", "b", "k") {
		t.Fatalf("no-property edge must report absent")
	}
	// Unknown endpoints / unknown key all report absent (never panic).
	if g.EdgeHasProperty("nope", "b", "k") {
		t.Fatalf("unknown src must report absent")
	}
	if g.EdgeHasProperty("a", "nada", "k") {
		t.Fatalf("unknown dst must report absent")
	}
	if err := g.SetEdgeProperty("a", "b", "present", Int64Value(1)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if g.EdgeHasProperty("a", "b", "never-interned") {
		t.Fatalf("never-interned key must report absent")
	}
}

// TestGraph_EdgeHasProperty_Directed confirms presence is directional: storage
// holds (a->b), so the (b->a) probe must report absent. This mirrors
// GetEdgeProperty and is what lets the Cypher reverse-hop pass query the
// correct stored direction.
func TestGraph_EdgeHasProperty_Directed(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if !g.EdgeHasProperty("a", "b", "since") {
		t.Fatalf("forward direction must report present")
	}
	if g.EdgeHasProperty("b", "a", "since") {
		t.Fatalf("reverse direction (no stored edge) must report absent")
	}
}

// TestGraph_EdgeHasProperty_LatestKindWins changes the value kind on the same
// pair: a present non-null value, then overwritten by a null-mapping kind
// (PropBytes / PropTime), must flip EdgeHasProperty to false — and back to true
// when overwritten again by a non-null kind. This pins the C12 congruence with
// EdgeProperties' latest-wins coalescing on a single slot (where set clears the
// prior different-kind column).
func TestGraph_EdgeHasProperty_LatestKindWins(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "k", Int64Value(1)); err != nil {
		t.Fatalf("set int: %v", err)
	}
	if !g.EdgeHasProperty("a", "b", "k") {
		t.Fatalf("int value must report present")
	}
	// Overwrite with a null-mapping kind: the coalesced winner now reads as Null
	// through Cypher, so EdgeHasProperty must report absent.
	if err := g.SetEdgeProperty("a", "b", "k", BytesValue([]byte{9})); err != nil {
		t.Fatalf("set bytes: %v", err)
	}
	if g.EdgeHasProperty("a", "b", "k") {
		t.Fatalf("after overwrite with bytes (null-mapping), must report absent")
	}
	// Cross-check with the value path: GetEdgeProperty sees the bytes (present in
	// storage) but its kind maps to Null, which is exactly why presence is false.
	if v, ok := g.GetEdgeProperty("a", "b", "k"); !ok || v.Kind() != PropBytes {
		t.Fatalf("storage should still hold the bytes value, got ok=%v kind=%v", ok, v.Kind())
	}
	// Overwrite again with a non-null kind: presence returns.
	if err := g.SetEdgeProperty("a", "b", "k", StringValue("back")); err != nil {
		t.Fatalf("set string: %v", err)
	}
	if !g.EdgeHasProperty("a", "b", "k") {
		t.Fatalf("after overwrite with string, must report present again")
	}
}

// TestGraph_EdgeHasProperty_ZeroAlloc verifies the presence check allocates
// nothing on the hot path (it reads only validity bits and kind tags, never the
// value cell). NOT parallel so testing.AllocsPerRun is meaningful.
func TestGraph_EdgeHasProperty_ZeroAlloc(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// A date value is the worst case: its value path (GetEdgeProperty/
	// slotValue) would build a tagged string, so a zero-alloc presence check
	// proves the value cell is never read.
	if err := g.SetEdgeProperty("a", "b", "k", StringValue(dateTagged("2020-01-01"))); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if allocs := testing.AllocsPerRun(200, func() {
		_ = g.EdgeHasProperty("a", "b", "k")
	}); allocs != 0 {
		t.Fatalf("EdgeHasProperty allocated %v objects/op, want 0", allocs)
	}
	// Absent-key probe must also be zero-alloc.
	if allocs := testing.AllocsPerRun(200, func() {
		_ = g.EdgeHasProperty("a", "b", "absent")
	}); allocs != 0 {
		t.Fatalf("EdgeHasProperty(absent) allocated %v objects/op, want 0", allocs)
	}
}
