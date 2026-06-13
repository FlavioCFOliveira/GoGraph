package dot_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
)

// TestWrite_IsolatedNodesOnly asserts that a graph made up entirely of
// vertices with no incident edges still emits every node id, rather than
// collapsing to an empty graph body (#1439).
func TestWrite_IsolatedNodesOnly(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	ids := []string{"alpha", "beta", "gamma"}
	for _, id := range ids {
		if err := a.AddNode(id); err != nil {
			t.Fatalf("AddNode %q: %v", id, err)
		}
	}

	var buf bytes.Buffer
	if err := dot.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	for _, id := range ids {
		if !strings.Contains(out, id+";") {
			t.Errorf("DOT output missing bare statement for isolated node %q:\n%s", id, out)
		}
	}
}

// TestWrite_IsolatedNodeAlongsideEdges asserts an isolated vertex survives
// even when other vertices are connected, and that connected vertices are
// not duplicated as redundant bare statements (#1439).
func TestWrite_IsolatedNodeAlongsideEdges(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("x", "y", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddNode("lonely"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var buf bytes.Buffer
	if err := dot.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "x -> y;") {
		t.Errorf("DOT output missing edge x -> y:\n%s", out)
	}
	if !strings.Contains(out, "lonely;") {
		t.Errorf("DOT output missing isolated node lonely:\n%s", out)
	}
	// Connected vertices must not also appear as bare statements.
	if strings.Contains(out, "  x;\n") || strings.Contains(out, "  y;\n") {
		t.Errorf("connected vertex emitted as redundant bare statement:\n%s", out)
	}
}
