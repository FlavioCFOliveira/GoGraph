package graphml_test

import (
	"bytes"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/io/graphml"
	"gograph/internal/shapegen"
)

// TestGraphMLRoundtrip_DegenerateGraphs verifies that the GraphML
// writer and reader handle edge cases correctly: the empty graph
// and a graph consisting solely of isolated nodes.
func TestGraphMLRoundtrip_DegenerateGraphs(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()

		a := adjlist.New[string, int64](adjlist.Config{Directed: true})

		var buf bytes.Buffer
		if err := graphml.Write(&buf, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, _, err := graphml.ReadInto(&buf)
		if err != nil {
			t.Fatalf("ReadInto: %v", err)
		}
		if got.Order() != 0 {
			t.Errorf("Order: got %d, want 0", got.Order())
		}
		if got.Size() != 0 {
			t.Errorf("Size: got %d, want 0", got.Size())
		}
	})

	t.Run("isolated_only", func(t *testing.T) {
		t.Parallel()

		g, err := shapegen.IsolatedOnly(5).Build(adjlist.Config{})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		sa := toStringAdj(g.AdjList())

		var buf bytes.Buffer
		if err := graphml.Write(&buf, sa); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, _, err := graphml.ReadInto(&buf)
		if err != nil {
			t.Fatalf("ReadInto: %v", err)
		}
		if got.Order() != 5 {
			t.Errorf("Order: got %d, want 5", got.Order())
		}
		if got.Size() != 0 {
			t.Errorf("Size: got %d, want 0", got.Size())
		}
	})
}
