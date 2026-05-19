package graphml

import (
	"strings"
	"testing"
)

func TestReadInto_Basic(t *testing.T) {
	t.Parallel()
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
  <key id="w" for="edge" attr.name="weight" attr.type="long"/>
  <graph id="G" edgedefault="directed">
    <node id="alice"/>
    <node id="bob"/>
    <node id="carol"/>
    <edge source="alice" target="bob"><data key="w">7</data></edge>
    <edge source="bob" target="carol"><data key="w">9</data></edge>
  </graph>
</graphml>`
	a, n, err := ReadInto(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 2 {
		t.Fatalf("edges added = %d, want 2", n)
	}
	if !a.HasEdge("alice", "bob") || !a.HasEdge("bob", "carol") {
		t.Fatalf("missing edge")
	}
}

func TestReadInto_Undirected(t *testing.T) {
	t.Parallel()
	doc := `<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
<graph edgedefault="undirected"><node id="a"/><node id="b"/><edge source="a" target="b"/></graph>
</graphml>`
	a, _, err := ReadInto(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasEdge("a", "b") || !a.HasEdge("b", "a") {
		t.Fatalf("undirected: mirror edge missing")
	}
}

func TestReadInto_NoGraph(t *testing.T) {
	t.Parallel()
	doc := `<graphml xmlns="http://graphml.graphdrawing.org/xmlns"></graphml>`
	a, n, err := ReadInto(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || a == nil {
		t.Fatalf("empty graph: n=%d a=%v", n, a)
	}
}

func TestReadInto_BadXML(t *testing.T) {
	t.Parallel()
	_, _, err := ReadInto(strings.NewReader("<garbage"))
	if err == nil {
		t.Fatalf("expected parse error")
	}
}
