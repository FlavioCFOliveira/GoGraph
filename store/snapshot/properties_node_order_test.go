package snapshot

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestWriteProperties_NodeRecordOrderIsDeterministic pins rmp #2519 on the
// snapshot side: a node's property records are emitted in ascending key-table
// index order, so properties.bin is a deterministic function of the graph.
//
// The fixture forces both property-bag tiers into the map representation that
// iterates in Go's randomised order: one node carries more than smallBagMax
// properties (lpg's smallBagMax), and one carries a list (a kind the stream tier does not model)
// beside two scalars. Before the fix those records followed map order, so the
// ascending-order assertion failed and repeated writes of the same graph
// differed byte for byte.
func TestWriteProperties_NodeRecordOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"wide", "listy", "small"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	// 24 is three times the lpg property bag's inline capacity (smallBagMax,
	// 8), so this node's bag is certainly promoted to the map tier.
	const wide = 24
	for i := 0; i < wide; i++ {
		if err := g.SetNodeProperty("wide", fmt.Sprintf("k%02d", i), lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for _, kv := range []struct {
		k string
		v lpg.PropertyValue
	}{
		{"k00", lpg.StringValue("a")},
		{"lst", lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)})},
		{"k01", lpg.BoolValue(true)},
	} {
		if err := g.SetNodeProperty("listy", kv.k, kv.v); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.SetNodeProperty("small", "k03", lpg.Int64Value(3)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	write := func() []byte {
		var buf bytes.Buffer
		if _, _, err := WriteProperties(&buf, g, nil); err != nil {
			t.Fatalf("WriteProperties: %v", err)
		}
		return buf.Bytes()
	}
	first := write()

	rb, err := ReadProperties(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("ReadProperties: %v", err)
	}
	perNode := map[uint64]int{}
	for i := 1; i < len(rb.NodeProperties); i++ {
		prev, cur := rb.NodeProperties[i-1], rb.NodeProperties[i]
		if prev.NodeID == cur.NodeID && prev.KeyIdx >= cur.KeyIdx {
			t.Fatalf("node %d: record %d has KeyIdx %d after %d; records must ascend by key index",
				cur.NodeID, i, cur.KeyIdx, prev.KeyIdx)
		}
	}
	for _, r := range rb.NodeProperties {
		perNode[r.NodeID]++
	}
	// Non-vacuity: the map-tier nodes were really written, with every record.
	if got := len(rb.NodeProperties); got != wide+3+1 {
		t.Fatalf("node property records = %d, want %d", got, wide+3+1)
	}
	if len(perNode) != 3 {
		t.Fatalf("records cover %d nodes, want 3", len(perNode))
	}

	for i := 0; i < 8; i++ {
		if again := write(); !bytes.Equal(first, again) {
			t.Fatalf("repeat write %d of the same graph produced different properties.bin bytes", i+1)
		}
	}
}
