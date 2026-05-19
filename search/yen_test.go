package search

import (
	"testing"
)

func TestYen_KShortest(t *testing.T) {
	t.Parallel()
	// Two-path fixture: 0->1->3 (cost 4), 0->2->3 (cost 3).
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := YenKShortest(c, src, dst, 2)
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2", len(got))
	}
	if got[0].Cost > got[1].Cost {
		t.Fatalf("paths not sorted by cost")
	}
	if got[0].Cost != 3 {
		t.Fatalf("first path cost = %d, want 3", got[0].Cost)
	}
}

func TestYen_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := YenKShortest(c, src, dst, 3); len(got) != 0 {
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestYen_KZero(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(1)
	if got := YenKShortest(c, src, dst, 0); got != nil {
		t.Fatalf("k=0 must return nil")
	}
}
