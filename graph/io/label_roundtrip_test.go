package io_test

// label_roundtrip_test.go — regression gate for #1793 (sprint 250): node labels
// used to be silently dropped by every export format, so export->import lost
// them. JSONL and GraphML now carry node labels and restore them on import.

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func labelledGraph(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, k := range []string{"alice", "bob", "carol"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	// alice has two labels (incl. one with a comma to prove robust encoding),
	// bob has one, carol has none.
	for _, l := range []string{"Person", "Admin,Staff"} {
		if err := g.SetNodeLabel("alice", l); err != nil {
			t.Fatalf("SetNodeLabel alice %q: %v", l, err)
		}
	}
	if err := g.SetNodeLabel("bob", "Person"); err != nil {
		t.Fatalf("SetNodeLabel bob: %v", err)
	}
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	return g
}

func assertLabels(t *testing.T, g *lpg.Graph[string, int64]) {
	t.Helper()
	check := func(node string, want []string) {
		got := g.NodeLabels(node)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("node %q labels = %v, want %v", node, got, want)
		}
	}
	check("alice", []string{"Person", "Admin,Staff"})
	check("bob", []string{"Person"})
	check("carol", nil)
}

func TestJSONL_NodeLabelsRoundTrip_1793(t *testing.T) {
	var buf bytes.Buffer
	if _, err := jsonl.WriteWithProps(&buf, labelledGraph(t)); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	g2, _, err := jsonl.ReadWithProps(&buf, adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	assertLabels(t, g2)
}

func TestGraphML_NodeLabelsRoundTrip_1793(t *testing.T) {
	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, labelledGraph(t)); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	g2, _, err := graphml.ReadWithProps(&buf)
	if err != nil {
		t.Fatalf("ReadWithProps: %v (out:\n%s)", err, buf.String())
	}
	assertLabels(t, g2)
}

func TestLabelLessOutputUnchanged_1793(t *testing.T) {
	// A graph with no labels must not emit any label encoding (byte-stable,
	// back-compatible output).
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	_ = g.AddNode("x")
	_ = g.AddNode("y")
	_ = g.AddEdge("x", "y", 1)

	var jb bytes.Buffer
	if _, err := jsonl.WriteWithProps(&jb, g); err != nil {
		t.Fatalf("jsonl WriteWithProps: %v", err)
	}
	if strings.Contains(jb.String(), "labels") {
		t.Errorf("label-less JSONL must not mention labels; got:\n%s", jb.String())
	}

	var gb bytes.Buffer
	if err := graphml.WriteWithProps(&gb, g); err != nil {
		t.Fatalf("graphml WriteWithProps: %v", err)
	}
	if strings.Contains(gb.String(), `id="labels"`) {
		t.Errorf("label-less GraphML must not emit the label key; got:\n%s", gb.String())
	}
}
