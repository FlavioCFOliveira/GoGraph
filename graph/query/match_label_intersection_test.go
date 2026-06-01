package query_test

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// setupLabelIntersectionGraph builds a 2000-node graph with three
// label bands:
//
//	"A": nodes 0..999
//	"B": nodes 500..1499
//	"C": nodes 1000..1999
//
// Intersections (node keys):
//
//	A∩B  = 500..999   (500 nodes)
//	A∩C  = empty
//	B∩C  = 1000..1499 (500 nodes)
//	A∩B∩C = empty
func setupLabelIntersectionGraph(tb testing.TB) (*lpg.Graph[int, int64], *csr.CSR[int64]) {
	tb.Helper()

	g := lpg.New[int, int64](adjlist.Config{Directed: true})

	for i := range 2000 {
		if i < 1000 {
			if err := g.SetNodeLabel(i, "A"); err != nil {
				tb.Fatalf("SetNodeLabel A node %d: %v", i, err)
			}
		}
		if i >= 500 && i < 1500 {
			if err := g.SetNodeLabel(i, "B"); err != nil {
				tb.Fatalf("SetNodeLabel B node %d: %v", i, err)
			}
		}
		if i >= 1000 {
			if err := g.SetNodeLabel(i, "C"); err != nil {
				tb.Fatalf("SetNodeLabel C node %d: %v", i, err)
			}
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	return g, c
}

// oracle returns the sorted intersection of all provided [lo, hi)
// half-open ranges of integer node keys.
func oracleIntersect(ranges [][2]int) []int {
	if len(ranges) == 0 {
		return nil
	}
	// Start with the first range as a set.
	lo, hi := ranges[0][0], ranges[0][1]
	result := make([]int, 0, hi-lo)
	for i := lo; i < hi; i++ {
		result = append(result, i)
	}
	for _, r := range ranges[1:] {
		filtered := result[:0]
		for _, v := range result {
			if v >= r[0] && v < r[1] {
				filtered = append(filtered, v)
			}
		}
		result = filtered
	}
	return result
}

func TestQuery_LabelIntersection(t *testing.T) {
	t.Parallel()

	g, c := setupLabelIntersectionGraph(t)
	e := query.New(g, c)

	// Label ranges: A=[0,1000), B=[500,1500), C=[1000,2000)
	rangeA := [2]int{0, 1000}
	rangeB := [2]int{500, 1500}
	rangeC := [2]int{1000, 2000}

	cases := []struct {
		name   string
		labels []string
		oracle [][2]int
	}{
		{
			name:   "A∩B",
			labels: []string{"A", "B"},
			oracle: [][2]int{rangeA, rangeB},
		},
		{
			name:   "A∩C",
			labels: []string{"A", "C"},
			oracle: [][2]int{rangeA, rangeC},
		},
		{
			name:   "B∩C",
			labels: []string{"B", "C"},
			oracle: [][2]int{rangeB, rangeC},
		},
		{
			name:   "A∩B∩C",
			labels: []string{"A", "B", "C"},
			oracle: [][2]int{rangeA, rangeB, rangeC},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preds := make([]query.Predicate[int, int64], len(tc.labels))
			for i, l := range tc.labels {
				preds[i] = query.WithLabel[int, int64](l)
			}

			got := e.Match().Vertex(preds...).Collect()
			slices.Sort(got)

			want := oracleIntersect(tc.oracle)

			// Cardinality must match oracle first — cheap check.
			wantCard := uint64(len(want))
			gotCard := e.Match().Vertex(preds...).Cardinality()
			if gotCard != wantCard {
				t.Fatalf("Cardinality = %d, want %d", gotCard, wantCard)
			}

			if len(got) != len(want) {
				t.Fatalf("len(Collect()) = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("got[%d] = %d, want %d", i, got[i], want[i])
				}
			}
		})
	}
}
