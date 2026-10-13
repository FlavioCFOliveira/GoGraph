package dot

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestWrite_Directed(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{})
	if err := a.AddEdge("alice", "bob", 5); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("bob", "carol", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "digraph G {") {
		t.Fatalf("missing digraph header: %q", out)
	}
	if !strings.Contains(out, "alice -> bob") {
		t.Fatalf("missing alice -> bob: %q", out)
	}
	if !strings.Contains(out, `[label="5"]`) {
		t.Fatalf("missing weight label: %q", out)
	}
	if strings.Contains(out, "label=") && strings.Contains(out, "bob -> carol [label") {
		t.Fatalf("zero-weight edge should omit label: %q", out)
	}
}

func TestWrite_QuotesSpecialIDs(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{})
	if err := a.AddEdge("ali ce", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ali ce"`) {
		t.Fatalf("ID with space should be quoted: %q", buf.String())
	}
}
