package cypher

// reltype_colsize_probe_test.go — the EXACT footprint of the type column, so the
// end-to-end retained-memory reading has a structural number to be checked
// against rather than only a difference of two noisy heap samples.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestRelTypeColumnSize reports the column's own bytes for the fixture the
// retained-memory probe uses, and asserts the structural invariant that makes it
// predictable: 4 bytes per arc per direction, and NO patch list for a graph whose
// arcs each carry one type.
func TestRelTypeColumnSize(t *testing.T) {
	for _, n := range []int{25_000, 100_000} {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("n%d", i)
			if err := g.AddNode(k); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeLabel(k, "P"); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < n; i++ {
			src := fmt.Sprintf("n%d", i)
			for j, typ := range []string{"K", "M"} {
				dst := fmt.Sprintf("n%d", (i+j+1)%n)
				if err := g.AddEdge(src, dst, 1); err != nil {
					t.Fatal(err)
				}
				g.SetEdgeLabel(src, dst, typ)
			}
		}
		view := g.ReadAt(nil)
		fwd, rev := csrPairFromGraph(view)
		col := buildRelTypeColumn(view, fwd, rev)
		arcs := len(fwd.EdgesSlice())
		got := col.RelTypeColumnBytes()
		want := int64(arcs) * 4 * 2 // dense fwd + dense rev, 4 bytes each
		if got != want {
			t.Errorf("column bytes = %d, want %d (%d arcs × 4 B × 2 directions) — a "+
				"difference means a patch list was allocated for a graph whose arcs "+
				"each carry exactly one relationship type", got, want, arcs)
		}
		t.Logf("COLUMNSIZE arcs=%d bytes=%d (%.2f B/arc, both directions)",
			arcs, got, float64(got)/float64(arcs))
	}
}
