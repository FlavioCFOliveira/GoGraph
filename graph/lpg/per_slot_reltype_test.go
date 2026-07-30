package lpg_test

// per_slot_reltype_test.go — rmp #2258 at the lpg layer: a relationship's TYPE
// belongs to the relationship INSTANCE, so a multigraph pair's parallel slots each
// carry their own type and the typed degree counts slots, not pairs.
//
// Every expectation is a HAND-COMPUTED ABSOLUTE count against a fixture built one
// call at a time. The pre-existing differential in outdegree_test.go compares the
// degree against an enumeration of the same label column, so it cannot see a
// defect in what the column CONTAINS — only in how it is read. These tests assert
// the contents.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// perSlotGraph returns a directed multigraph with nodes a, b, c and no edges.
func perSlotGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
	}
	return g
}

// typedDegree is OutDegreeByType with the type resolved by name, and 0 for a type
// the graph never interned.
func typedDegree(t *testing.T, g *lpg.Graph[string, float64], src, relType string) int {
	t.Helper()
	lid, known := g.Registry().Lookup(relType)
	if !known {
		return 0
	}
	got, ok := g.OutDegreeByType(src, lid)
	if !ok {
		t.Fatalf("OutDegreeByType(%q, %s) reported not-interned", src, relType)
	}
	return got
}

// TestSetEdgeLabel_TypesEveryFreeParallelSlot is the write-path core: SetEdgeLabel
// names the PAIR, so every free slot of it becomes a relationship of that type.
// Placing the type on the first free slot alone left the rest at the 0 sentinel and
// a typed degree counted 1 where the pair held two or three such relationships.
func TestSetEdgeLabel_TypesEveryFreeParallelSlot(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 8} {
		t.Run(fmt.Sprintf("slots=%d", n), func(t *testing.T) {
			t.Parallel()
			g := perSlotGraph(t)
			for i := 0; i < n; i++ {
				if err := g.AddEdge("a", "b", 1); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
			g.SetEdgeLabel("a", "b", "K")

			if got, _ := g.OutDegree("a"); got != n {
				t.Fatalf("fixture: OutDegree(a) = %d, want %d", got, n)
			}
			if got := typedDegree(t, g, "a", "K"); got != n {
				t.Errorf("OutDegreeByType(a, K) = %d, want %d", got, n)
			}
			// The derived per-pair set is unchanged by the per-slot placement: the
			// pair still carries exactly one distinct type.
			if got := g.EdgeLabels("a", "b"); len(got) != 1 || got[0] != "K" {
				t.Errorf("EdgeLabels(a, b) = %v, want [K]", got)
			}
			if !g.HasEdgeLabel("a", "b", "K") {
				t.Error("HasEdgeLabel(a, b, K) = false, want true")
			}
		})
	}
}

// TestAddEdgeLabeled_TypesOnlyItsOwnSlot is the complement: the labelled-build fast
// path types the slot it appends and no other, so an untyped parallel sibling stays
// untyped. Together with the test above this is what makes the two construction
// sequences distinguishable.
func TestAddEdgeLabeled_TypesOnlyItsOwnSlot(t *testing.T) {
	t.Parallel()
	g := perSlotGraph(t)
	if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if got, _ := g.OutDegree("a"); got != 2 {
		t.Fatalf("fixture: OutDegree(a) = %d, want 2", got)
	}
	if got := typedDegree(t, g, "a", "K"); got != 1 {
		t.Errorf("OutDegreeByType(a, K) = %d, want 1 (only the AddEdgeLabeled slot is a :K)", got)
	}
}

// TestSetEdgeLabel_TwoTypesPerSlotVersusPerPair pins the distinction the per-slot
// storage buys, with a hand-computed count for each build order.
func TestSetEdgeLabel_TwoTypesPerSlotVersusPerPair(t *testing.T) {
	t.Parallel()
	t.Run("interleaved gives one relationship of each type", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		if err := g.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel("a", "b", "K")
		if err := g.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel("a", "b", "M")

		if got := typedDegree(t, g, "a", "K"); got != 1 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 1", got)
		}
		if got := typedDegree(t, g, "a", "M"); got != 1 {
			t.Errorf("OutDegreeByType(a, M) = %d, want 1", got)
		}
	})

	t.Run("both slots present gives two relationships carrying both types", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		for i := 0; i < 2; i++ {
			if err := g.AddEdge("a", "b", 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		g.SetEdgeLabel("a", "b", "K")
		g.SetEdgeLabel("a", "b", "M")

		// K took both free slots; M then found none free, so it applies to the pair
		// — which, SetEdgeLabel naming the pair, means to both of its slots.
		if got := typedDegree(t, g, "a", "K"); got != 2 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 2", got)
		}
		if got := typedDegree(t, g, "a", "M"); got != 2 {
			t.Errorf("OutDegreeByType(a, M) = %d, want 2", got)
		}
	})

	t.Run("one slot named twice carries both types", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		if err := g.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel("a", "b", "K")
		g.SetEdgeLabel("a", "b", "M")

		if got := typedDegree(t, g, "a", "K"); got != 1 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 1", got)
		}
		// M has no free slot and lives in the pair's overflow list, which the one
		// column-typed slot carries. A count that ignored overflow reported 0.
		if got := typedDegree(t, g, "a", "M"); got != 1 {
			t.Errorf("OutDegreeByType(a, M) = %d, want 1", got)
		}
	})

	t.Run("different types via AddEdgeLabeled twice", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled(K): %v", err)
		}
		if err := g.AddEdgeLabeled("a", "b", 1, "M"); err != nil {
			t.Fatalf("AddEdgeLabeled(M): %v", err)
		}
		if got := typedDegree(t, g, "a", "K"); got != 1 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 1", got)
		}
		if got := typedDegree(t, g, "a", "M"); got != 1 {
			t.Errorf("OutDegreeByType(a, M) = %d, want 1", got)
		}
	})
}

// TestSelfLoop_PerSlotRelType covers the self-loop shape, where every parallel slot
// of the pair has the SAME endpoint on both sides and a per-pair reading therefore
// spreads one type across the whole degree.
func TestSelfLoop_PerSlotRelType(t *testing.T) {
	t.Parallel()
	t.Run("eleven untyped slots and one typed slot", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		for i := 0; i < 11; i++ {
			if err := g.AddEdge("a", "a", 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		if err := g.AddEdgeLabeled("a", "a", 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled: %v", err)
		}
		if got, _ := g.OutDegree("a"); got != 12 {
			t.Fatalf("fixture: OutDegree(a) = %d, want 12", got)
		}
		if got := typedDegree(t, g, "a", "K"); got != 1 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 1", got)
		}
	})

	t.Run("twelve slots all named", func(t *testing.T) {
		t.Parallel()
		g := perSlotGraph(t)
		for i := 0; i < 12; i++ {
			if err := g.AddEdge("a", "a", 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		g.SetEdgeLabel("a", "a", "K")
		if got := typedDegree(t, g, "a", "K"); got != 12 {
			t.Errorf("OutDegreeByType(a, K) = %d, want 12", got)
		}
	})
}

// TestRemoveEdgeLabel_ClearsEveryParallelSlot is the exact inverse of
// TestSetEdgeLabel_TypesEveryFreeParallelSlot. Clearing the first matching slot
// alone left the pair's other slots still reporting the type.
func TestRemoveEdgeLabel_ClearsEveryParallelSlot(t *testing.T) {
	t.Parallel()
	g := perSlotGraph(t)
	for i := 0; i < 3; i++ {
		if err := g.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.SetEdgeLabel("a", "b", "K")
	g.SetEdgeLabel("a", "b", "M") // no free slot: applies to the pair via overflow

	if got := typedDegree(t, g, "a", "K"); got != 3 {
		t.Fatalf("precondition: OutDegreeByType(a, K) = %d, want 3", got)
	}

	g.RemoveEdgeLabel("a", "b", "K")
	if got := typedDegree(t, g, "a", "K"); got != 0 {
		t.Errorf("after RemoveEdgeLabel(K): OutDegreeByType(a, K) = %d, want 0", got)
	}
	if g.HasEdgeLabel("a", "b", "K") {
		t.Error("after RemoveEdgeLabel(K): HasEdgeLabel = true, want false")
	}
	// M was in overflow and is untouched by removing K.
	if got := typedDegree(t, g, "a", "M"); got != 3 {
		t.Errorf("after RemoveEdgeLabel(K): OutDegreeByType(a, M) = %d, want 3", got)
	}
	// The edges themselves survive.
	if got, _ := g.OutDegree("a"); got != 3 {
		t.Errorf("after RemoveEdgeLabel(K): OutDegree(a) = %d, want 3", got)
	}

	g.RemoveEdgeLabel("a", "b", "M")
	if got := typedDegree(t, g, "a", "M"); got != 0 {
		t.Errorf("after RemoveEdgeLabel(M): OutDegreeByType(a, M) = %d, want 0", got)
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Errorf("after both removals: EdgeLabels(a, b) = %v, want empty", got)
	}
}

// TestPerSlotRelType_BoundedAgreesWithUnbounded pins the bounded/unbounded contract
// at the lpg layer over the multigraph shapes. The unbounded form used to take a
// column-only fast path that consulted neither the per-handle store nor overflow, so
// it could disagree with the bounded form about WHICH edges count.
func TestPerSlotRelType_BoundedAgreesWithUnbounded(t *testing.T) {
	t.Parallel()
	builds := []struct {
		name  string
		build func(t *testing.T, g *lpg.Graph[string, float64])
		wantK int
	}{
		{"two named parallel slots", func(t *testing.T, g *lpg.Graph[string, float64]) {
			for i := 0; i < 2; i++ {
				if err := g.AddEdge("a", "b", 1); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
			g.SetEdgeLabel("a", "b", "K")
		}, 2},
		{"one typed and one untyped slot", func(t *testing.T, g *lpg.Graph[string, float64]) {
			if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
				t.Fatalf("AddEdgeLabeled: %v", err)
			}
			if err := g.AddEdge("a", "b", 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}, 1},
		{"type only in overflow", func(t *testing.T, g *lpg.Graph[string, float64]) {
			if err := g.AddEdge("a", "b", 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel("a", "b", "M")
			g.SetEdgeLabel("a", "b", "K")
		}, 1},
	}
	for _, b := range builds {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			g := perSlotGraph(t)
			b.build(t, g)
			lid, known := g.Registry().Lookup("K")
			if !known {
				t.Fatal("relationship type K was not interned")
			}
			if got := typedDegree(t, g, "a", "K"); got != b.wantK {
				t.Errorf("OutDegreeByType(a, K) = %d, want %d", got, b.wantK)
			}
			for _, limit := range []int{1, 2, 3, 1 << 20} {
				want := min(b.wantK, limit)
				if got, ok := g.OutDegreeByTypeBounded("a", lid, limit); !ok || got != want {
					t.Errorf("OutDegreeByTypeBounded(a, K, %d) = (%d, %v), want (%d, true)",
						limit, got, ok, want)
				}
			}
		})
	}
}
