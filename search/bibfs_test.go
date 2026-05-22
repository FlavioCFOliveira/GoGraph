package search

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBiBFS_Chain(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 9; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(9)
	path, err := BiBFS(c, src, dst)
	if err != nil {
		t.Fatalf("BiBFS: %v", err)
	}
	if len(path) != 10 {
		t.Fatalf("path length = %d, want 10", len(path))
	}
}

func TestBiBFS_SameSrcDst(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	path, err := BiBFS(c, src, src)
	if err != nil || len(path) != 1 {
		t.Fatalf("self-path = %v err=%v", path, err)
	}
}

func TestBiBFS_NoPath(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	_, err := BiBFS(c, src, dst)
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("expected ErrNoPath, got %v", err)
	}
}

// TestBiBFS_Directed verifies BiBFS on a directed graph: BiBFSCtx
// auto-builds the reverse CSR so the backward search walks in-edges.
func TestBiBFS_Directed(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(2)
	path, err := BiBFS(c, src, dst)
	if err != nil {
		t.Fatalf("BiBFS directed: %v", err)
	}
	if len(path) != 3 || path[0] != src || path[len(path)-1] != dst {
		t.Fatalf("path = %v, want src ... dst with length 3", path)
	}
}

// TestBiBFS_DirectedNoReversePath verifies that on a directed graph
// where the forward path exists but the reverse doesn't (1->0
// exists, 0->2 doesn't), BiBFS still recovers the correct directed
// path src=0 -> dst=2 via the auto-built reverse adjacency.
func TestBiBFS_DirectedNoReversePath(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	path, err := BiBFS(c, src, dst)
	if err != nil {
		t.Fatalf("BiBFS directed-chain: %v", err)
	}
	if len(path) != 4 {
		t.Fatalf("path length = %d, want 4", len(path))
	}
}
