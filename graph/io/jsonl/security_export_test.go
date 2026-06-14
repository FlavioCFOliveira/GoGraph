package jsonl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// Security regression pin for the JSONL writer's export neutralisation.
// Unlike the GraphML writer (which rejects characters XML cannot
// represent), the JSONL writer can carry arbitrary bytes losslessly because
// encoding/json escapes control characters and quotes. This test pins that
// a node id containing control characters and quote/backslash metacharacters
// survives a Write → ReadInto round-trip byte-for-byte — proving the
// escaping is lossless and that crafted bytes cannot break out of their JSON
// string to inject a spurious record.
func TestSec_IO_JSONLExportEscapesHostileID(t *testing.T) {
	t.Parallel()

	// A node id with a control char, a double quote, a backslash, and what
	// looks like an injected JSON record. encoding/json must escape all of
	// it inside one string token.
	hostile := "evil\x01\"}\n{\"type\":\"node\",\"id\":\"injected\\"

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddNode(hostile); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var buf bytes.Buffer
	if _, err := jsonl.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The raw newline inside the id must have been escaped (\n), never
	// emitted as a literal record separator that would split the line.
	if strings.Count(strings.TrimRight(buf.String(), "\n"), "\n") != 0 {
		t.Errorf("hostile id was not escaped — output contains a raw newline:\n%q", buf.String())
	}

	// Round-trip: the exact bytes must come back, with no "injected" node.
	got, _, err := jsonl.ReadInto(bytes.NewReader(buf.Bytes()), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if got.Order() != 1 {
		t.Fatalf("Order = %d, want 1 (no injected record)", got.Order())
	}
	if _, ok := got.Mapper().Lookup(hostile); !ok {
		t.Errorf("hostile id did not round-trip byte-for-byte")
	}
	if _, ok := got.Mapper().Lookup("injected"); ok {
		t.Errorf("a spurious 'injected' node appeared — JSON-string breakout")
	}
}
