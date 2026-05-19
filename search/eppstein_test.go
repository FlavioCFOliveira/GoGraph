package search

import (
	"testing"
)

func TestEppstein_KShortest(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := EppsteinKShortest(c, src, dst, 2)
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2", len(got))
	}
	if got[0].Cost != 3 {
		t.Fatalf("first path cost = %d, want 3", got[0].Cost)
	}
	if got[1].Cost != 4 {
		t.Fatalf("second path cost = %d, want 4", got[1].Cost)
	}
}

func TestEppstein_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := EppsteinKShortest(c, src, dst, 3); len(got) != 0 {
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestEppstein_AgreesWithYen(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
		{0, 3, 10},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	yen := YenKShortest(c, src, dst, 3)
	ep := EppsteinKShortest(c, src, dst, 3)
	if len(yen) != len(ep) {
		t.Fatalf("yen len=%d, eppstein len=%d", len(yen), len(ep))
	}
	for i := range yen {
		if yen[i].Cost != ep[i].Cost {
			t.Fatalf("path %d: yen cost=%d eppstein cost=%d", i, yen[i].Cost, ep[i].Cost)
		}
	}
}
