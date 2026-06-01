package search

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestHopcroftKarp_Isolated_RightSide builds a bipartite graph with
// |L|=20, |R|=30 where the last 10 right-side vertices (indices 20..29)
// are isolated (no edges to any left vertex). The remaining right
// vertices 0..19 form K_{20,20} with the left side.
//
// Expected results:
//   - Matching size == 20 (all left vertices saturated).
//   - Isolated right vertices 20..29 do not appear in the matching.
func TestHopcroftKarp_Isolated_RightSide(t *testing.T) {
	t.Parallel()
	const mLeft, nRight, nIsolated = 20, 30, 10
	const nActive = nRight - nIsolated // 20

	// Pre-intern left vertices then all right vertices (including isolated).
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < mLeft; i++ {
		if err := a.AddNode(fmt.Sprintf("L%05d", i)); err != nil {
			t.Fatalf("AddNode L%05d: %v", i, err)
		}
	}
	for i := 0; i < nRight; i++ {
		if err := a.AddNode(fmt.Sprintf("R%05d", i)); err != nil {
			t.Fatalf("AddNode R%05d: %v", i, err)
		}
	}
	// K_{20,20}: every left connects to every active right (0..19).
	for l := 0; l < mLeft; l++ {
		for r := 0; r < nActive; r++ {
			if err := a.AddEdge(fmt.Sprintf("L%05d", l), fmt.Sprintf("R%05d", r), struct{}{}); err != nil {
				t.Fatalf("AddEdge L%05d->R%05d: %v", l, r, err)
			}
		}
	}
	// Right vertices nActive..nRight-1 have no edges (isolated).

	c := csr.BuildFromAdjList(a)
	match := HopcroftKarp(c, int(c.MaxNodeID()))

	if match.Size != mLeft {
		t.Fatalf("matching size = %d, want %d", match.Size, mLeft)
	}

	// Collect NodeIDs for isolated right vertices so we can verify they
	// are absent from the matching. The mapper assigns NodeIDs
	// sequentially in order of first insertion; left nodes were
	// interned first, so right nodes follow.
	mapper := a.Mapper()
	for i := nActive; i < nRight; i++ {
		nodeID, ok := mapper.Lookup(fmt.Sprintf("R%05d", i))
		if !ok {
			t.Fatalf("R%05d was not interned in the mapper", i)
		}
		// MatchR[nodeID] should remain the unmatched sentinel (^NodeID(0) == MaxUint64).
		if uint64(nodeID) < uint64(len(match.MatchR)) {
			if int64(match.MatchR[nodeID]) >= 0 {
				t.Errorf("isolated right vertex R%05d (NodeID %d) appears in matching", i, nodeID)
			}
		}
	}
}
