package graphml

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestWriteWithProps_ManyLabelsByteReproducible pins the label half of the
// rmp #2519 writer audit: a node carrying more labels than the label bag's
// inline tier holds keeps them in a Go map, and the writer used to emit them in
// that map's iteration order, so two exports of the same graph differed. The
// labels are now sorted, as the property keys already were.
func TestWriteWithProps_ManyLabelsByteReproducible(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for k := 0; k < 12; k++ {
		if err := g.SetNodeLabel("n", fmt.Sprintf("L%02d", 11-k)); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	export := func() []byte {
		var b bytes.Buffer
		if err := WriteWithProps(&b, g); err != nil {
			t.Fatalf("WriteWithProps: %v", err)
		}
		return b.Bytes()
	}
	first := export()
	if !bytes.Contains(first, []byte(`L00`)) || !bytes.Contains(first, []byte(`L11`)) {
		t.Fatal("the export carries no labels; the comparison would be vacuous")
	}
	for i := 0; i < 8; i++ {
		if again := export(); !bytes.Equal(first, again) {
			t.Fatalf("export %d of the same graph differs byte for byte from the first", i+2)
		}
	}
}
