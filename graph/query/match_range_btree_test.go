package query_test

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// funcPredicate adapts an inline function to the query.Predicate interface.
// Used to express ad-hoc predicates (e.g., range checks) in tests
// without requiring engine-level range support.
type funcPredicate[N comparable, W any] struct {
	fn func(*lpg.Graph[N, W], graph.NodeID) bool
}

func (p funcPredicate[N, W]) Match(g *lpg.Graph[N, W], id graph.NodeID) bool {
	return p.fn(g, id)
}

// ageRange returns a query.Predicate that accepts nodes whose "age"
// Int64 property satisfies lo <= age <= hi.
func ageRange(lo, hi int64) query.Predicate[int, int64] {
	return funcPredicate[int, int64]{
		fn: func(g *lpg.Graph[int, int64], id graph.NodeID) bool {
			n, ok := g.AdjList().Mapper().Resolve(id)
			if !ok {
				return false
			}
			v, ok := g.GetNodeProperty(n, "age")
			if !ok {
				return false
			}
			age, ok := v.Int64()
			if !ok {
				return false
			}
			return age >= lo && age <= hi
		},
	}
}

// setupRangeGraph builds a 2000-node graph where:
//   - node i has "age" = int64(i % 101)   → values in [0, 100]
//   - nodes 0..999 carry label "Person"
func setupRangeGraph(tb testing.TB) (*lpg.Graph[int, int64], *csr.CSR[int64]) {
	tb.Helper()

	g := lpg.New[int, int64](adjlist.Config{Directed: true})

	for i := range 2000 {
		age := int64(i % 101)
		if err := g.SetNodeProperty(i, "age", lpg.Int64Value(age)); err != nil {
			tb.Fatalf("SetNodeProperty node %d: %v", i, err)
		}
		if i < 1000 {
			if err := g.SetNodeLabel(i, "Person"); err != nil {
				tb.Fatalf("SetNodeLabel node %d: %v", i, err)
			}
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	return g, c
}

// oracleRangePersonAge returns nodes (keys) that are "Person"
// (i < 1000) AND have age = i%101 in [lo, hi].
func oracleRangePersonAge(lo, hi int64) []int {
	out := make([]int, 0, 256)
	for i := range 1000 {
		age := int64(i % 101)
		if age >= lo && age <= hi {
			out = append(out, i)
		}
	}
	return out
}

func TestQuery_RangeBTree(t *testing.T) {
	t.Parallel()

	g, c := setupRangeGraph(t)
	e := query.New(g, c)

	cases := []struct {
		name   string
		lo, hi int64
	}{
		{"age [20,40]", 20, 40},
		{"age [0,0]", 0, 0},
		{"age [100,100]", 100, 100},
		{"age [50,60]", 50, 60},
		{"age [101,200]", 101, 200}, // above max age=100 → empty
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := oracleRangePersonAge(tc.lo, tc.hi)
			slices.Sort(want)

			got := e.Match().
				Vertex(
					query.WithLabel[int, int64]("Person"),
					ageRange(tc.lo, tc.hi),
				).
				Collect()
			slices.Sort(got)

			// Cardinality check.
			gotCard := e.Match().
				Vertex(
					query.WithLabel[int, int64]("Person"),
					ageRange(tc.lo, tc.hi),
				).
				Cardinality()
			wantCard := uint64(len(want))
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
