package graphml

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"gograph/graph/adjlist"
)

// BenchmarkWriteGraphML_1M measures the GraphML writer on a 1M-node
// graph with light fanout (~3 edges/node). Task #138 targets >2x
// speedup over the per-Resolve baseline by batching name resolution
// once per shard via Mapper.Walk.
func BenchmarkWriteGraphML_1M(b *testing.B) {
	const n = 1_000_000
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := a.AddEdge(fmt.Sprintf("n%07d", i), fmt.Sprintf("n%07d", (i+1)%n), 1); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Write(io.Discard, a)
	}
}

func TestWrite_Roundtrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("alice", "bob", 7); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("bob", "carol", 9); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	doc := buf.String()
	if !strings.Contains(doc, "graphml") {
		t.Fatalf("output missing graphml root")
	}
	b, n, err := ReadInto(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 2 {
		t.Fatalf("edges = %d, want 2", n)
	}
	if !b.HasEdge("alice", "bob") || !b.HasEdge("bob", "carol") {
		t.Fatalf("missing edge after roundtrip")
	}
}

func TestWrite_Undirected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: false})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `edgedefault="undirected"`) {
		t.Fatalf("missing edgedefault=undirected attribute")
	}
}
