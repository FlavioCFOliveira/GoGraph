package jsonl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestReadInto_Basic(t *testing.T) {
	t.Parallel()
	in := `{"type":"node","id":"alice"}
{"type":"node","id":"bob"}
{"type":"edge","src":"alice","dst":"bob","weight":7}
`
	a, n, err := ReadInto(strings.NewReader(in), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	if !a.HasEdge("alice", "bob") {
		t.Fatalf("missing alice -> bob")
	}
}

func TestReadInto_BadJSON(t *testing.T) {
	t.Parallel()
	_, _, err := ReadInto(strings.NewReader("not json\n"), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestReadInto_UnknownType(t *testing.T) {
	t.Parallel()
	_, _, err := ReadInto(strings.NewReader(`{"type":"alien"}`+"\n"), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatalf("expected unknown-type error")
	}
}

func TestReadInto_MissingFields(t *testing.T) {
	t.Parallel()
	if _, _, err := ReadInto(strings.NewReader(`{"type":"node"}`+"\n"), adjlist.Config{Directed: true}); err == nil {
		t.Fatalf("missing node id should error")
	}
	if _, _, err := ReadInto(strings.NewReader(`{"type":"edge","src":"a"}`+"\n"), adjlist.Config{Directed: true}); err == nil {
		t.Fatalf("missing edge dst should error")
	}
}

func TestWrite_Roundtrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("bob", "carol", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if _, err := Write(&buf, a); err != nil {
		t.Fatal(err)
	}
	b, _, err := ReadInto(&buf, adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if !b.HasEdge("alice", "bob") || !b.HasEdge("bob", "carol") {
		t.Fatalf("missing edge after roundtrip")
	}
}
