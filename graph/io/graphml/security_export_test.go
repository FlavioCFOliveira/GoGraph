package graphml_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Security regression pin for the GraphML writer's export neutralisation.
// XML 1.0 cannot represent the C0 control characters (other than tab,
// newline, and carriage return); Go's encoding/xml would silently replace
// them with U+FFFD, destroying the original bytes. The writer instead fails
// fast with the typed ErrInvalidXMLChar so a hostile or malformed id can
// never produce a corrupted-but-accepted document. This test pins that the
// fail-fast guard fires on a control character in a node id.
func TestSec_IO_GraphMLExportRejectsControlChar(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// U+0007 (BEL) is a C0 control character outside the XML 1.0 Char set.
	if err := g.AddNode("bad\x07id"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var buf bytes.Buffer
	err := graphml.WriteWithProps(&buf, g)
	if !errors.Is(err, graphml.ErrInvalidXMLChar) {
		t.Fatalf("err = %v, want ErrInvalidXMLChar", err)
	}
}
