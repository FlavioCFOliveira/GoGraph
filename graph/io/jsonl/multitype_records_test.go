package jsonl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestJSONL_MultiTypeRecords exercises the full WriteWithProps/ReadWithProps
// roundtrip covering node, edge, and property record types in a single stream.
func TestJSONL_MultiTypeRecords(t *testing.T) {
	t.Parallel()

	// Build the source graph.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	for _, node := range []string{"alice", "bob", "carol"} {
		if err := g.AddNode(node); err != nil {
			t.Fatalf("AddNode(%q): %v", node, err)
		}
	}

	edges := []struct {
		src, dst string
		w        int64
	}{
		{"alice", "bob", 5},
		{"bob", "carol", 3},
	}
	for _, e := range edges {
		if err := g.AddEdge(e.src, e.dst, e.w); err != nil {
			t.Fatalf("AddEdge(%q→%q): %v", e.src, e.dst, err)
		}
	}

	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty alice.age: %v", err)
	}
	if err := g.SetNodeProperty("bob", "name", lpg.StringValue("Robert")); err != nil {
		t.Fatalf("SetNodeProperty bob.name: %v", err)
	}
	if err := g.SetNodeProperty("carol", "active", lpg.BoolValue(true)); err != nil {
		t.Fatalf("SetNodeProperty carol.active: %v", err)
	}

	// Write.
	var buf bytes.Buffer
	n, err := jsonl.WriteWithProps(&buf, g)
	if err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	// 3 nodes + 2 edges + 3 properties = 8 records.
	if n != 8 {
		t.Errorf("WriteWithProps returned %d records, want 8", n)
	}
	// Sanity: every line must contain a "type" field.
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, `"type"`) {
			t.Errorf("line %d missing \"type\" field: %s", i, line)
		}
	}

	// Read back.
	g2, rows, err := jsonl.ReadWithProps(strings.NewReader(buf.String()), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if rows != 8 {
		t.Errorf("ReadWithProps consumed %d rows, want 8", rows)
	}

	// Verify topology.
	a := g2.AdjList()
	for _, node := range []string{"alice", "bob", "carol"} {
		if _, ok := a.Mapper().Lookup(node); !ok {
			t.Errorf("node %q missing after roundtrip", node)
		}
	}
	for _, e := range edges {
		if !a.HasEdge(e.src, e.dst) {
			t.Errorf("edge %q→%q missing after roundtrip", e.src, e.dst)
		}
	}

	// Verify properties.
	age, ok := g2.GetNodeProperty("alice", "age")
	if !ok {
		t.Fatal("alice.age missing after roundtrip")
	}
	if v, _ := age.Int64(); v != 30 {
		t.Errorf("alice.age = %d, want 30", v)
	}

	name, ok := g2.GetNodeProperty("bob", "name")
	if !ok {
		t.Fatal("bob.name missing after roundtrip")
	}
	if v, _ := name.String(); v != "Robert" {
		t.Errorf("bob.name = %q, want \"Robert\"", v)
	}

	active, ok := g2.GetNodeProperty("carol", "active")
	if !ok {
		t.Fatal("carol.active missing after roundtrip")
	}
	if v, _ := active.Bool(); !v {
		t.Errorf("carol.active = %v, want true", v)
	}
}
