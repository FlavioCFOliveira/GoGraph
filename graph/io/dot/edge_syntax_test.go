package dot_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
)

// TestDOTWrite_EdgeSyntax verifies directed vs undirected edge operators,
// weight label emission, and multi-edge output.
func TestDOTWrite_EdgeSyntax(t *testing.T) {
	t.Parallel()

	t.Run("directed_arrow", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddEdge("a", "b", 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		var buf bytes.Buffer
		if err := dot.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "->") {
			t.Errorf("directed graph must use -> operator:\n%s", out)
		}
		if !strings.Contains(out, "digraph") {
			t.Errorf("directed graph must emit digraph header:\n%s", out)
		}
	})

	t.Run("undirected_dash", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: false})
		if err := a.AddEdge("a", "b", 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		var buf bytes.Buffer
		if err := dot.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "--") {
			t.Errorf("undirected graph must use -- operator:\n%s", out)
		}
		if !strings.Contains(out, "graph G {") {
			t.Errorf("undirected graph must emit 'graph G {' header:\n%s", out)
		}
		if strings.Contains(out, "digraph") {
			t.Errorf("undirected graph must not emit digraph header:\n%s", out)
		}
	})

	t.Run("nonzero_weight_label", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddEdge("a", "b", 42); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		var buf bytes.Buffer
		if err := dot.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `label="42"`) {
			t.Errorf("non-zero weight must emit label attribute:\n%s", out)
		}
	})

	t.Run("zero_weight_no_label", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddEdge("a", "b", 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		var buf bytes.Buffer
		if err := dot.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "label=") {
			t.Errorf("zero-weight edge must not emit label attribute:\n%s", out)
		}
	})

	t.Run("multi_edge", func(t *testing.T) {
		t.Parallel()
		// 3 edges forming a path: a -> b -> c -> d
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		edges := [][2]string{{"a", "b"}, {"b", "c"}, {"c", "d"}}
		for _, e := range edges {
			if err := a.AddEdge(e[0], e[1], 0); err != nil {
				t.Fatalf("AddEdge %v->%v: %v", e[0], e[1], err)
			}
		}
		var buf bytes.Buffer
		if err := dot.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := buf.String()
		count := strings.Count(out, "->")
		if count != 3 {
			t.Errorf("expected 3 edge lines, got %d:\n%s", count, out)
		}
	})
}
