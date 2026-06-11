package jsonl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestJSONL_TombstonedNodeNotExported is the regression gate for the
// consistency violation where WriteWithProps serialised tombstoned
// (logically removed) nodes as live records, so an export→import round
// trip resurrected deleted data. A node removed via lpg.Graph.RemoveNode
// must not appear in the output, and neither must any edge or property
// record referencing it; LiveOrder must be preserved across the round
// trip.
func TestJSONL_TombstonedNodeNotExported(t *testing.T) {
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
	if _, err := jsonl.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()

	// Audit repro: the deleted node must not be emitted as a live record.
	if strings.Contains(out, `{"type":"node","id":"dead"}`) {
		t.Fatalf("tombstoned node exported as a live node record:\n%s", out)
	}
	// No edge or property record may reference the deleted node either.
	if strings.Contains(out, `"dead"`) {
		t.Fatalf("output references the tombstoned node:\n%s", out)
	}

	imported, _, err := jsonl.ReadWithProps(strings.NewReader(out), adjlist.Config{Directed: true})
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

// TestJSONL_DeleteThenRecreateExportsOnce verifies that a node deleted
// and then re-created (which clears its tombstone) is exported exactly
// once, as a live node.
func TestJSONL_DeleteThenRecreateExportsOnce(t *testing.T) {
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
	if _, err := jsonl.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, `{"type":"node","id":"dead"}`); got != 1 {
		t.Fatalf("revived node exported %d times, want 1:\n%s", got, out)
	}

	imported, _, err := jsonl.ReadWithProps(strings.NewReader(out), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got, want := imported.LiveOrder(), uint64(1); got != want {
		t.Fatalf("LiveOrder after round trip = %d, want %d", got, want)
	}
	if _, ok := imported.AdjList().Mapper().Lookup("dead"); !ok {
		t.Fatal("revived node missing after round trip")
	}
}

// TestJSONL_OnlyTombstonedNodesExportsNothing verifies that a graph in
// which every node has been removed serialises to an empty record
// stream that imports back as an empty graph.
func TestJSONL_OnlyTombstonedNodesExportsNothing(t *testing.T) {
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
	written, err := jsonl.WriteWithProps(&buf, g)
	if err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	if written != 0 {
		t.Fatalf("written = %d records for a fully-tombstoned graph, want 0:\n%s", written, buf.String())
	}

	imported, _, err := jsonl.ReadWithProps(strings.NewReader(buf.String()), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got := imported.LiveOrder(); got != 0 {
		t.Fatalf("LiveOrder after round trip = %d, want 0", got)
	}
}
