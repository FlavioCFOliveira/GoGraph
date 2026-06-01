package extern

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

func TestBFS_Chain(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 9; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "chain.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	src, _ := a.Mapper().Lookup(0)
	depths := map[graph.NodeID]int{}
	_ = BFS(r, src, func(node graph.NodeID, d int) bool {
		depths[node] = d
		return true
	})

	for i := 0; i < 10; i++ {
		id, _ := a.Mapper().Lookup(i)
		if depths[id] != i {
			t.Fatalf("node %d depth = %d, want %d", i, depths[id], i)
		}
	}
}

func TestBFS_EarlyStop(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 100; i++ {
		if err := a.AddEdge(0, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "star.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	src, _ := a.Mapper().Lookup(0)
	count := 0
	_ = BFS(r, src, func(_ graph.NodeID, _ int) bool {
		count++
		return count < 5
	})
	if count != 5 {
		t.Fatalf("early-stop count = %d", count)
	}
}

func TestBFS_UnknownSrc(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "tiny.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	count := 0
	_ = BFS(r, graph.NodeID(1<<30), func(_ graph.NodeID, _ int) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("BFS from unknown src visited %d nodes", count)
	}
}
