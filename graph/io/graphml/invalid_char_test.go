package graphml_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// TestWrite_InvalidNodeID_FailStop asserts that the plain (non-props)
// GraphML writer also fails fast with ErrInvalidXMLChar on a node id
// carrying an XML-illegal control character, rather than letting
// encoding/xml silently substitute U+FFFD (#1437).
func TestWrite_InvalidNodeID_FailStop(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddNode("ctrl\x01\x1f"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var buf bytes.Buffer
	err := graphml.Write(&buf, a)
	if !errors.Is(err, graphml.ErrInvalidXMLChar) {
		t.Fatalf("Write err = %v, want ErrInvalidXMLChar", err)
	}
	if strings.ContainsRune(buf.String(), '�') {
		t.Errorf("GraphML emitted a U+FFFD substitution: %q", buf.String())
	}
}

// TestWrite_CleanNodeID_OK is the control: a well-formed id writes fine.
func TestWrite_CleanNodeID_OK(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("alice", "bob", 7); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if err := graphml.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `id="alice"`) {
		t.Errorf("output missing node alice:\n%s", buf.String())
	}
}
