package recovery

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// twoSided returns a graph holding the nodes a and b, interned in that order so
// a's id is the lower, for a hand-built two-sided legacy image.
func twoSided(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	m := g.AdjList().Mapper()
	if a, _ := m.Lookup("a"); func() bool { b, _ := m.Lookup("b"); return a >= b }() {
		t.Fatal("a is not interned below b")
	}
	return g
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func addArc(t *testing.T, g *lpg.Graph[string, int64], src, dst string, w int64, h uint64) {
	t.Helper()
	if _, err := g.AddEdgeHIfAbsent(src, dst, w, h); err != nil {
		t.Fatalf("AddEdgeHIfAbsent: %v", err)
	}
}

// TestFoldLegacyUndirected_UnpairedArcIsKept pins F4 of the rmp #3072 audit:
// a higher→lower arc with no mirror (b -> a, handle 2, weight 9) is re-oriented
// a -> b and kept with its handle, weight and records, never dropped; the
// mirrored edge (handle 1) is kept once.
func TestFoldLegacyUndirected_UnpairedArcIsKept(t *testing.T) {
	t.Parallel()
	g := twoSided(t)
	addArc(t, g, "a", "b", 4, 1)
	addArc(t, g, "b", "a", 4, 1)
	addArc(t, g, "b", "a", 9, 2)
	mustDo(t, g.SetEdgePropertyByHandle("b", "a", 2, "k", lpg.Int64Value(7)))
	mustDo(t, foldLegacyUndirected(g))
	hs := g.AppendEdgeHandles("a", "b", nil)
	slices.Sort(hs)
	if !slices.Equal(hs, []uint64{1, 2}) || len(g.AppendEdgeHandles("b", "a", nil)) != 0 || g.AdjList().Size() != 2 {
		t.Fatalf("a->b %v b->a %v size %d, want a->b [1 2], no b->a, size 2",
			hs, g.AppendEdgeHandles("b", "a", nil), g.AdjList().Size())
	}
	var weights []int64
	for _, w := range g.AdjList().Neighbours("a") {
		weights = append(weights, w)
	}
	slices.Sort(weights)
	if !slices.Equal(weights, []int64{4, 9}) {
		t.Errorf("a -> b weights = %v, want [4 9]", weights)
	}
	if v, ok := g.EdgePropertyByHandle("a", "b", 2, "k"); !ok || !reflect.DeepEqual(v, lpg.Int64Value(7)) {
		t.Errorf("relationship 2 k = %v (ok=%v), want 7", v, ok)
	}
}

// TestFoldLegacyUndirected_MergesTheReverseDirection pins that what only the
// higher→lower direction holds — a pair property, a relationship type in the
// arc's label column, a handle-keyed type and property — is kept on the folded
// relationship, and nothing stays keyed b -> a.
func TestFoldLegacyUndirected_MergesTheReverseDirection(t *testing.T) {
	t.Parallel()
	g := twoSided(t)
	addArc(t, g, "a", "b", 0, 1)
	addArc(t, g, "b", "a", 0, 1)
	mustDo(t, g.SetEdgeProperty("a", "b", "x", lpg.Int64Value(1)))
	mustDo(t, g.SetEdgeProperty("b", "a", "y", lpg.Int64Value(2)))
	mustDo(t, g.SetEdgeLabel("b", "a", "T"))
	mustDo(t, g.SetEdgeLabelByHandle("b", "a", 1, "H"))
	mustDo(t, g.SetEdgePropertyByHandle("b", "a", 1, "z", lpg.Int64Value(3)))
	mustDo(t, foldLegacyUndirected(g))
	want := map[string]lpg.PropertyValue{"x": lpg.Int64Value(1), "y": lpg.Int64Value(2)}
	if got := g.EdgeProperties("a", "b"); !reflect.DeepEqual(got, want) {
		t.Errorf("a -> b properties = %v, want %v", got, want)
	}
	if got := g.EdgeLabels("a", "b"); !slices.Equal(got, []string{"T"}) {
		t.Errorf("a -> b types = %v, want [T]", got)
	}
	if got := g.EdgeLabelsByHandle("a", "b", 1); !slices.Equal(got, []string{"H"}) {
		t.Errorf("relationship 1 types = %v, want [H]", got)
	}
	if v, ok := g.EdgePropertyByHandle("a", "b", 1, "z"); !ok || !reflect.DeepEqual(v, lpg.Int64Value(3)) {
		t.Errorf("relationship 1 z = %v (ok=%v), want 3", v, ok)
	}
	if len(g.EdgeLabels("b", "a")) != 0 || len(g.EdgeProperties("b", "a")) != 0 ||
		len(g.EdgeLabelsByHandle("b", "a", 1)) != 0 || len(g.EdgePropertiesByHandle("b", "a", 1)) != 0 {
		t.Error("a record stayed keyed b -> a")
	}
}

// TestFoldLegacyUndirected_RefusesBeforeChanging pins that a conflict is
// reported before the graph is changed: both directions survive.
func TestFoldLegacyUndirected_RefusesBeforeChanging(t *testing.T) {
	t.Parallel()
	g := twoSided(t)
	addArc(t, g, "a", "b", 0, 1)
	addArc(t, g, "b", "a", 0, 1)
	mustDo(t, g.SetEdgePropertyByHandle("a", "b", 1, "w", lpg.StringValue("x")))
	mustDo(t, g.SetEdgePropertyByHandle("b", "a", 1, "w", lpg.StringValue("y")))
	if err := foldLegacyUndirected(g); !errors.Is(err, ErrLegacyMirrorConflict) {
		t.Fatalf("err = %v, want ErrLegacyMirrorConflict", err)
	}
	if g.AdjList().Size() != 2 || !g.HasEdgeHandle("b", "a", 1) {
		t.Fatal("a refused fold changed the graph")
	}
}

// TestSameLegacyValue pins the value equality the conflict rule uses.
func TestSameLegacyValue(t *testing.T) {
	t.Parallel()
	l := func(v ...lpg.PropertyValue) lpg.PropertyValue { return lpg.ListValue(v) }
	for _, tc := range []struct {
		a, b lpg.PropertyValue
		same bool
	}{
		{lpg.Int64Value(1), lpg.Int64Value(1), true},
		{lpg.Int64Value(1), lpg.Int64Value(2), false},
		{lpg.Int64Value(1), lpg.Float64Value(1), false},
		{lpg.StringValue("a"), lpg.StringValue("a"), true},
		{l(lpg.Int64Value(1)), l(lpg.Int64Value(1)), true},
		{l(lpg.Int64Value(1)), l(lpg.Int64Value(2)), false},
		{lpg.BytesValue([]byte{1}), lpg.BytesValue([]byte{1}), true},
	} {
		if got := sameLegacyValue(tc.a, tc.b); got != tc.same {
			t.Errorf("sameLegacyValue(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}
