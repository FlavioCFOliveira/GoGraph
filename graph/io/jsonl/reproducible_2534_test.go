package jsonl

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestWriteWithProps_ByteReproducible pins rmp #2534: two exports of the same
// graph are byte-identical. Before the fix the writer ranged over each node's
// property map, so the property records came out in Go's randomised order. The
// fixture also gives one node more labels than the label bag's inline tier
// holds, so the labels come back from a Go map too.
func TestWriteWithProps_ByteReproducible(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{})
	for i := 0; i < 4; i++ {
		n := fmt.Sprintf("n%d", i)
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		for k := 0; k < 12; k++ {
			if err := g.SetNodeProperty(n, fmt.Sprintf("p%02d", 11-k), lpg.Int64Value(int64(k))); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	for k := 0; k < 12; k++ {
		if err := g.SetNodeLabel("n0", fmt.Sprintf("L%02d", 11-k)); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if err := g.AddEdge("n0", "n1", 7); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	export := func() string {
		var b bytes.Buffer
		if _, err := WriteWithProps(&b, g); err != nil {
			t.Fatalf("WriteWithProps: %v", err)
		}
		return b.String()
	}
	first := export()
	if got := strings.Count(first, `"type":"property"`); got != 48 {
		t.Fatalf("export carries %d property records, want 48", got)
	}
	// The order is the documented one: ascending by key within a node.
	if i, j := strings.Index(first, `"key":"p00"`), strings.Index(first, `"key":"p01"`); i < 0 || j < 0 || i > j {
		t.Fatalf("property records are not in ascending key order (p00 at %d, p01 at %d)", i, j)
	}
	for i := 0; i < 8; i++ {
		if again := export(); again != first {
			t.Fatalf("export %d of the same graph differs byte for byte from the first", i+2)
		}
	}

	// The export still round-trips: order is presentation, not content.
	back, _, err := ReadWithProps(strings.NewReader(first), adjlist.Config{})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	if got := len(back.NodeProperties("n3")); got != 12 {
		t.Fatalf("round-tripped n3 carries %d properties, want 12", got)
	}
	if got := len(back.NodeLabels("n0")); got != 12 {
		t.Fatalf("round-tripped n0 carries %d labels, want 12", got)
	}
}
