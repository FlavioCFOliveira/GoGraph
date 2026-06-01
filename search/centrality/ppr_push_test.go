package centrality

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestPPR_SourceCarriesMostMass(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 9; i++ {
		if err := a.AddEdge(0, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(i+1, 0, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	r, _ := PersonalisedPushPageRank(c, src, DefaultPPRPushOptions())
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
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	r, _ := PersonalisedPushPageRank(c, 9999, DefaultPPRPushOptions())
	if r != nil {
		t.Fatalf("PPR from unknown src should return nil")
	}
}

// TestPPR_BoundedMassAtSource asserts the dangling-teleport fix keeps
// mass at the source bounded above the leaves: ACL with dangling
// teleport sends absorbed alpha-mass back to src, so the src rank
// strictly dominates any leaf.
func TestPPR_BoundedMassAtSource(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 1; i <= 5; i++ {
		if err := a.AddEdge(0, i, struct{}{}); err != nil { // src to dangling leaves
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	r, _ := PersonalisedPushPageRank(c, src, DefaultPPRPushOptions())
	for i := 1; i <= 5; i++ {
		leaf, _ := a.Mapper().Lookup(i)
		if r[src] <= r[leaf] {
			t.Fatalf("src rank %.6f should exceed dangling leaf %d rank %.6f",
				r[src], i, r[leaf])
		}
	}
	// Total absorbed rank (without residue) should be in (0, 1].
	var total float64
	for _, v := range r {
		total += v
	}
	if total <= 0 || total > 1.0+1e-9 {
		t.Fatalf("rank sum = %.6f, want in (0, 1]", total)
	}
}

func TestPPR_RejectsNaN(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	_, err := PersonalisedPushPageRank(c, src, PPRPushOptions{Damping: math.NaN()})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
