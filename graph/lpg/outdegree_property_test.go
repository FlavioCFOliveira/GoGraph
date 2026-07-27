package lpg_test

// outdegree_property_test.go — the degree/enumeration identity holds on
// randomly generated graphs, not only on hand-picked shapes (task #2218,
// acceptance criterion 4).
//
// The hand-written differential in outdegree_test.go covers the cases a human
// thought to cover. This one generates the graph — node count, edge list,
// relationship types, which nodes get tombstoned, and the graph configuration —
// and asserts the same identity for EVERY node on EVERY generated graph:
//
//	OutDegree(n)            == the number of live out-neighbours a traversal yields
//	OutDegreeByType(n, t)   == the same, restricted to relationship type t
//	sum over all types      <= OutDegree(n)      (untyped edges make it a <=)
//
// Generating the tombstone set matters: it is the input that separates the O(1)
// fast path from the filtered path, and a property test is the cheapest way to
// exercise both across many shapes.

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestOutDegreeProperty_MatchesEnumeration is the generative differential.
func TestOutDegreeProperty_MatchesEnumeration(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		directed := rapid.Bool().Draw(rt, "directed")
		multigraph := rapid.Bool().Draw(rt, "multigraph")
		nodeCount := rapid.IntRange(1, 12).Draw(rt, "nodeCount")
		edgeCount := rapid.IntRange(0, 30).Draw(rt, "edgeCount")
		typeCount := rapid.IntRange(1, 3).Draw(rt, "typeCount")

		g := lpg.New[string, float64](adjlist.Config{Directed: directed, Multigraph: multigraph})

		nodes := make([]string, nodeCount)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%d", i)
			if err := g.AddNode(nodes[i]); err != nil {
				rt.Fatalf("AddNode: %v", err)
			}
		}

		types := make([]string, typeCount)
		for i := range types {
			types[i] = fmt.Sprintf("T%d", i)
		}

		for e := 0; e < edgeCount; e++ {
			src := nodes[rapid.IntRange(0, nodeCount-1).Draw(rt, fmt.Sprintf("src%d", e))]
			dst := nodes[rapid.IntRange(0, nodeCount-1).Draw(rt, fmt.Sprintf("dst%d", e))]
			// Some edges are typed and some are not, so the per-type sum is a
			// lower bound on the total rather than equal to it.
			if rapid.Bool().Draw(rt, fmt.Sprintf("typed%d", e)) {
				rel := types[rapid.IntRange(0, typeCount-1).Draw(rt, fmt.Sprintf("rel%d", e))]
				if err := g.AddEdgeLabeled(src, dst, 1, rel); err != nil {
					// A simple graph refuses a duplicate pair; that is a legal
					// outcome, not a test failure.
					continue
				}
				continue
			}
			if err := g.AddEdge(src, dst, 1); err != nil {
				continue
			}
		}

		// Tombstone an arbitrary subset, which is what forces the filtered path.
		for _, n := range nodes {
			if rapid.Bool().Draw(rt, "tombstone_"+n) {
				g.RemoveNode(n)
			}
		}

		for _, n := range nodes {
			want := enumerateOutDegree(g, n)
			got, ok := g.OutDegree(n)
			if !ok {
				rt.Fatalf("%s: OutDegree reported not-interned for an interned node", n)
			}
			if got != want {
				rt.Fatalf("%s: OutDegree = %d, enumeration = %d (directed=%v multigraph=%v)",
					n, got, want, directed, multigraph)
			}

			typedTotal := 0
			for _, rel := range types {
				lid, known := g.Registry().Lookup(rel)
				if !known {
					continue // this type was never used on this generated graph
				}
				wantT := enumerateOutDegreeByType(g, n, rel)
				gotT, okT := g.OutDegreeByType(n, lid)
				if !okT {
					rt.Fatalf("%s/%s: OutDegreeByType reported not-interned", n, rel)
				}
				if gotT != wantT {
					rt.Fatalf("%s/%s: OutDegreeByType = %d, enumeration = %d",
						n, rel, gotT, wantT)
				}
				typedTotal += gotT
			}
			if typedTotal > got {
				rt.Fatalf("%s: typed degrees sum to %d, exceeding the total degree %d",
					n, typedTotal, got)
			}
		}
	})
}
