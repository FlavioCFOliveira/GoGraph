package search

import "testing"

func TestFloydWarshall_HandBuilt(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	cases := map[int]int64{0: 0, 1: 7, 2: 3, 3: 8, 4: 5}
	for k, expected := range cases {
		id, _ := a.Mapper().Lookup(k)
		v, ok := apsp.At(src, id)
		if !ok || v != expected {
			t.Fatalf("d(0,%d) = (%d, %v), want %d", k, v, ok, expected)
		}
	}
}

func TestFloydWarshall_Unreachable(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if _, ok := apsp.At(src, dst); ok {
		t.Fatalf("(0,3) should be unreachable")
	}
}
