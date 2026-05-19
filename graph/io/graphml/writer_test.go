package graphml

import (
	"bytes"
	"strings"
	"testing"

	"gograph/graph/adjlist"
)

func TestWrite_Roundtrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("alice", "bob", 7)
	a.AddEdge("bob", "carol", 9)

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
	a.AddEdge("a", "b", 1)
	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `edgedefault="undirected"`) {
		t.Fatalf("missing edgedefault=undirected attribute")
	}
}
