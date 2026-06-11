package lpg_test

// removeedge_hygiene_test.go — gate test for clearEdgePairState completeness.
//
// Verifies that RemoveEdge removes the last edge between (src, dst) and
// clears ALL three previously-missing sidecar stores:
//   - edgeInstanceLabelShards  (SetEdgeLabelAt / EdgeLabelsAt)
//   - edgeInstancePropShards   (SetEdgePropertyAt / EdgePropertiesAt)
//   - edgeCreateCountShards    (IncEdgeCreateCount / EdgeCreateCount)
//
// The test must FAIL before the fix (stale state survives) and pass after.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// assertEdgeClean verifies that all three instance sidecar stores are empty
// for the directed edge (a→b) at instance index 1.
func assertEdgeClean(t *testing.T, g *lpg.Graph[int, int64], a, b int, when string) {
	t.Helper()

	if got := g.EdgeLabelsAt(a, b, 1); len(got) != 0 {
		t.Errorf("%s: EdgeLabelsAt(%d,%d,1) = %v; want empty", when, a, b, got)
	}
	if got := g.EdgeCreateCount(a, b); got != 0 {
		t.Errorf("%s: EdgeCreateCount(%d,%d) = %d; want 0", when, a, b, got)
	}
	if got := g.EdgePropertiesAt(a, b, 1); len(got) != 0 {
		t.Errorf("%s: EdgePropertiesAt(%d,%d,1) = %v; want nil/empty", when, a, b, got)
	}
}

func TestRemoveEdge_ClearsSidecarStores(t *testing.T) {
	t.Parallel()

	g := lpg.New[int, int64](adjlist.Config{Directed: true, Multigraph: false})

	const (
		a   = 1
		b   = 2
		idx = int64(1)
	)

	// --- setup: add nodes and one directed edge ---
	if err := g.AddNode(a); err != nil {
		t.Fatalf("AddNode(%d): %v", a, err)
	}
	if err := g.AddNode(b); err != nil {
		t.Fatalf("AddNode(%d): %v", b, err)
	}
	if err := g.AddEdge(a, b, 0); err != nil {
		t.Fatalf("AddEdge(%d,%d): %v", a, b, err)
	}

	// --- populate the three instance sidecar stores ---
	g.SetEdgeLabelAt(a, b, idx, "KNOWS")
	g.IncEdgeCreateCount(a, b) // bumps counter to 1
	if err := g.SetEdgePropertyAt(a, b, idx, "since", lpg.StringValue("2024")); err != nil {
		t.Fatalf("SetEdgePropertyAt: %v", err)
	}

	// Sanity: confirm all three stores are populated before removal.
	if got := g.EdgeLabelsAt(a, b, idx); len(got) == 0 {
		t.Fatal("pre-condition: EdgeLabelsAt returned empty before RemoveEdge")
	}
	if got := g.EdgeCreateCount(a, b); got == 0 {
		t.Fatal("pre-condition: EdgeCreateCount returned 0 before RemoveEdge")
	}
	if got := g.EdgePropertiesAt(a, b, idx); len(got) == 0 {
		t.Fatal("pre-condition: EdgePropertiesAt returned empty before RemoveEdge")
	}

	// --- remove the last (and only) edge ---
	g.RemoveEdge(a, b)

	// All three stores must be clean after removal.
	assertEdgeClean(t, g, a, b, "after RemoveEdge")

	// --- re-create the edge; it must also start with clean state ---
	if err := g.AddEdge(a, b, 0); err != nil {
		t.Fatalf("re-AddEdge(%d,%d): %v", a, b, err)
	}
	assertEdgeClean(t, g, a, b, "after re-AddEdge")
}
