package centrality

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestPPR_SourceCarriesMostMass(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 9; i++ {
		a.AddEdge(0, i+1, struct{}{})
		a.AddEdge(i+1, 0, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	r := PersonalisedPushPageRank(c, src, DefaultPPRPushOptions())
	maxIdx := 0
	for i, v := range r {
		if v > r[maxIdx] {
			maxIdx = i
		}
	}
	if uint64(maxIdx) != uint64(src) {
		t.Fatalf("max rank at %d, want src %d", maxIdx, src)
	}
}

func TestPPR_UnknownSrc(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, struct{}{})
	c := csr.BuildFromAdjList(a)
	r := PersonalisedPushPageRank(c, 9999, DefaultPPRPushOptions())
	if r != nil {
		t.Fatalf("PPR from unknown src should return nil")
	}
}
