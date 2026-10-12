package cypher

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pickEdgeTypeBySet is the selection rule as it stood before rmp #3106 (a set
// of the accepted names, then the first stored label in it), kept as the oracle
// the allocation-free scan must reproduce exactly.
func pickEdgeTypeBySet(stored, accepted []string) string {
	if len(stored) == 0 {
		return ""
	}
	if len(accepted) > 0 {
		set := make(map[string]struct{}, len(accepted))
		for _, a := range accepted {
			set[a] = struct{}{}
		}
		for _, s := range stored {
			if _, ok := set[s]; ok {
				return s
			}
		}
	}
	best := stored[0]
	for _, s := range stored[1:] {
		if s < best {
			best = s
		}
	}
	return best
}

// TestPickEdgeTypeMatchesTheSetRule pins the selection order when several
// accepted types match (the FIRST stored label wins, whatever the accepted
// list's order), the no-match fallback to the smallest stored label, and the
// empty cases.
func TestPickEdgeTypeMatchesTheSetRule(t *testing.T) {
	for _, tc := range []struct {
		stored, accepted []string
		want             string
	}{
		{nil, []string{"A"}, ""},
		{[]string{"B", "A"}, nil, "A"},
		{[]string{"B", "A"}, []string{"A", "B"}, "B"},
		{[]string{"A", "B"}, []string{"B", "A"}, "A"},
		{[]string{"C", "B", "A"}, []string{"A", "B"}, "B"},
		{[]string{"C", "D"}, []string{"A", "B"}, "C"},
		{[]string{"A"}, []string{"A", "A"}, "A"},
		{[]string{"Z", "Y"}, []string{"Q", "R", "S", "T", "U", "V", "W", "X", "Y"}, "Y"},
	} {
		got := pickEdgeType(tc.stored, tc.accepted)
		if got != tc.want || got != pickEdgeTypeBySet(tc.stored, tc.accepted) {
			t.Errorf("pickEdgeType(%v, %v) = %q, want %q (set rule %q)",
				tc.stored, tc.accepted, got, tc.want, pickEdgeTypeBySet(tc.stored, tc.accepted))
		}
	}
	if n := testing.AllocsPerRun(100, func() {
		_ = pickEdgeType([]string{"C", "B"}, []string{"A", "B"})
	}); n != 0 {
		t.Errorf("pickEdgeType allocated %.0f times per call with an accepted list, want 0", n)
	}
}

// TestMultiTypePairTypeIsUnchanged drives the materialiser end to end on a
// Go-API pair carrying two types under a multi-type pattern, so the type the
// row surfaces is the pair-union path through EdgeLabelsByID and pickEdgeType.
// The expected type is computed from the same stored order with the set rule.
func TestMultiTypePairTypeIsUnchanged(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"h", "t"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddEdgeLabeledWithProperty("h", "t", 1, "B", "w", lpg.Int64Value(1)); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdgeLabeledWithProperty("h", "t", 1, "A", "w", lpg.Int64Value(2)); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(g)
	stored := g.ReadAt(nil).EdgeLabels("h", "t")
	if len(stored) != 2 {
		t.Fatalf("fixture stored %v, want two types on the pair", stored)
	}
	for _, q := range []struct{ pattern, accepted []string }{
		{[]string{"A", "B"}, []string{"A", "B"}},
		{[]string{"B", "A"}, []string{"B", "A"}},
	} {
		query := "MATCH (h)-[r:" + q.pattern[0] + "|" + q.pattern[1] + "]->(t) RETURN DISTINCT type(r) AS ty"
		rows := drainRows(t, e, query)
		want := "ty=\"" + pickEdgeTypeBySet(stored, q.accepted) + "\""
		if len(rows) == 0 {
			t.Fatalf("%s: no rows", query)
		}
		for _, r := range rows {
			if r != want {
				t.Fatalf("%s: row %s, want %s (stored order %v)", query, r, want, stored)
			}
		}
	}
}
