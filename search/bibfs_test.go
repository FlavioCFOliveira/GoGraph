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
		a.AddEdge(i, i+1, struct{}{})
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
	a.AddEdge(0, 1, struct{}{})
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
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(2, 3, struct{}{})
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	_, err := BiBFS(c, src, dst)
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("expected ErrNoPath, got %v", err)
	}
}
