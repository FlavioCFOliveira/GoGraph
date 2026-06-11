package graphml_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestGraphML_TombstonedNodeNotExported is the regression gate for the
// consistency violation where WriteWithProps serialised tombstoned
// (logically removed) nodes as live <node> elements, so an
// export→import round trip resurrected deleted data. A node removed via
// lpg.Graph.RemoveNode must not appear in the output — neither as a
// <node> nor as an <edge> endpoint, nor through a <key> declaration that
// only its properties would justify — and LiveOrder must be preserved
// across the round trip.
func TestGraphML_TombstonedNodeNotExported(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b", "dead"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	for _, e := range []struct {
		src, dst string
		w        int64
	}{{"a", "dead", 1}, {"dead", "b", 2}, {"a", "b", 3}} {
		if err := g.AddEdge(e.src, e.dst, e.w); err != nil {
			t.Fatalf("AddEdge(%q, %q): %v", e.src, e.dst, err)
		}
	}
	if err := g.SetNodeProperty("dead", "ghost", lpg.StringValue("boo")); err != nil {
		t.Fatalf("SetNodeProperty(dead): %v", err)
	}
	if err := g.SetNodeProperty("a", "name", lpg.StringValue("alpha")); err != nil {
		t.Fatalf("SetNodeProperty(a): %v", err)
	}
	g.RemoveNode("dead")

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, `<node id="dead"`) {
		t.Fatalf("tombstoned node exported as a live <node>:\n%s", out)
	}
	if strings.Contains(out, `source="dead"`) || strings.Contains(out, `target="dead"`) {
		t.Fatalf("incident edge of the tombstoned node exported:\n%s", out)
	}
	// The "ghost" property exists only on the deleted node; its <key>
	// declaration must not leak into the document.
	if strings.Contains(out, `attr.name="ghost"`) {
		t.Fatalf("property key of the tombstoned node declared:\n%s", out)
	}
	if !strings.Contains(out, `attr.name="name"`) {
		t.Fatalf("property key of a live node missing:\n%s", out)
	}

	imported, _, err := graphml.ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got, want := imported.LiveOrder(), g.LiveOrder(); got != want {
		t.Fatalf("LiveOrder after round trip = %d, want %d", got, want)
	}
	if _, ok := imported.AdjList().Mapper().Lookup("dead"); ok {
		t.Fatal("deleted node resurrected by export→import round trip")
	}
	if !imported.AdjList().HasEdge("a", "b") {
		t.Fatal("live edge (a)->(b) lost in round trip")
	}
	if imported.AdjList().HasEdge("a", "dead") || imported.AdjList().HasEdge("dead", "b") {
		t.Fatal("incident edge of the deleted node resurrected by round trip")
	}
}

// TestGraphML_DeleteThenRecreateExportsOnce verifies that a node deleted
// and then re-created (which clears its tombstone) is exported exactly
// once, as a live <node>.
func TestGraphML_DeleteThenRecreateExportsOnce(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("dead"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.RemoveNode("dead")
	if err := g.AddNode("dead"); err != nil {
		t.Fatalf("re-AddNode: %v", err)
	}

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, `<node id="dead"`); got != 1 {
		t.Fatalf("revived node exported %d times, want 1:\n%s", got, out)
	}

	imported, _, err := graphml.ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got, want := imported.LiveOrder(), uint64(1); got != want {
		t.Fatalf("LiveOrder after round trip = %d, want %d", got, want)
	}
}

// TestGraphML_OnlyTombstonedNodesExportsEmptyGraph verifies that a graph
// in which every node has been removed serialises to a well-formed
// GraphML document with no <node> or <edge> elements, and imports back
// as an empty graph.
func TestGraphML_OnlyTombstonedNodesExportsEmptyGraph(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"x", "y"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	if err := g.AddEdge("x", "y", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.RemoveNode("x")
	g.RemoveNode("y")

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<node") || strings.Contains(out, "<edge") {
		t.Fatalf("fully-tombstoned graph exported nodes or edges:\n%s", out)
	}

	imported, edges, err := graphml.ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got := imported.LiveOrder(); got != 0 {
		t.Fatalf("LiveOrder after round trip = %d, want 0", got)
	}
	if edges != 0 {
		t.Fatalf("edges after round trip = %d, want 0", edges)
	}
}
