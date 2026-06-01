package search

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// BenchmarkHierholzer_LargeEulerian measures Hierholzer on a large
// Eulerian circuit (4*N edges through 1 cycle of length 4 repeated
// N times). Task #133 caps allocs/op at 2 (down from the v1.0
// behaviour where the trail slice grew via standard append).
func BenchmarkHierholzer_LargeEulerian(b *testing.B) {
	const n = 1 << 12 // 4k segments
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		base := i * 4
		if err := a.AddEdge(base, base+1, struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(base+1, base+2, struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(base+2, base+3, struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(base+3, base, struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Hierholzer(c)
	}
}

func TestHierholzer_Cycle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, (i+1)%4, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	trail, err := Hierholzer(c)
	if err != nil {
		t.Fatalf("Hierholzer: %v", err)
	}
	if len(trail) != 5 {
		t.Fatalf("trail length = %d, want 5 (4 edges + 1)", len(trail))
	}
	// circuit -> first == last
	if trail[0] != trail[len(trail)-1] {
		t.Fatalf("Eulerian circuit must close: trail[0]=%d trail[-1]=%d", trail[0], trail[len(trail)-1])
	}
}

func TestHierholzer_NoEulerianTwoSinks(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	_, err := Hierholzer(c)
	if !errors.Is(err, ErrNoEulerian) {
		t.Fatalf("expected ErrNoEulerian, got %v", err)
	}
}

func TestHierholzer_Disconnected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 3; i++ {
		if err := a.AddEdge(i, (i+1)%3, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for i := 10; i < 13; i++ {
		if err := a.AddEdge(i, (i+1)%3+10, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	if _, err := Hierholzer(c); !errors.Is(err, ErrNoEulerian) {
		t.Fatalf("disconnected graph should fail, got %v", err)
	}
}
