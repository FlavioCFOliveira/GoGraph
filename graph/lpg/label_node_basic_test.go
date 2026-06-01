package lpg_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// labelOracle tracks the expected set of labels per node so tests can
// assert HasNodeLabel bit-for-bit against the LPG.
type labelOracle map[graph.NodeID]map[string]bool

func (o labelOracle) set(id graph.NodeID, name string) {
	if o[id] == nil {
		o[id] = make(map[string]bool)
	}
	o[id][name] = true
}

func (o labelOracle) remove(id graph.NodeID, name string) {
	delete(o[id], name)
	if len(o[id]) == 0 {
		delete(o, id)
	}
}

func (o labelOracle) has(id graph.NodeID, name string) bool {
	return o[id][name]
}

// checkOracle verifies that every oracle entry matches HasNodeLabel exactly.
// It checks both positive and negative: for every (nodeID, name) pair in the
// oracle, HasNodeLabel must agree; for nodes not in the oracle HasNodeLabel
// must return false for a probe label that was never assigned.
func checkOracle(t *testing.T, tag string, g *lpg.Graph[int, int64], oracle labelOracle, allLabels []string, allIDs []graph.NodeID) {
	t.Helper()
	for _, id := range allIDs {
		nodeVal := int(id) // shapegen uses int keys matching their NodeID index
		for _, name := range allLabels {
			got := g.HasNodeLabel(nodeVal, name)
			want := oracle.has(id, name)
			if got != want {
				t.Errorf("%s: HasNodeLabel(nodeID=%d, %q) = %v, oracle = %v",
					tag, id, name, got, want)
			}
		}
	}
}

// exerciseShape runs the full label-mutation suite on a single graph built by
// shape. It is the shared body for Star, Complete, and CompleteBipartite sub-
// tests.
func exerciseShape(t *testing.T, shape shapegen.Shape[int, int64]) {
	t.Helper()

	g, err := shape.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Collect all NodeIDs and their corresponding user keys from the mapper.
	var allIDs []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ int) bool {
		allIDs = append(allIDs, id)
		return true
	})
	if len(allIDs) == 0 {
		t.Fatal("graph has no nodes")
	}

	// Label names used throughout the exercise. Using three labels exercises
	// multi-label bags as well as the last-label removal path.
	labels := []string{"Alpha", "Beta", "Gamma"}
	oracle := make(labelOracle)

	// --- Phase 1: set all three labels on every node ---
	for _, id := range allIDs {
		nodeVal := int(id)
		for _, name := range labels {
			if err := g.SetNodeLabel(nodeVal, name); err != nil {
				t.Fatalf("SetNodeLabel(%d, %q): %v", nodeVal, name, err)
			}
			oracle.set(id, name)
		}
	}
	checkOracle(t, "after-set-all", g, oracle, labels, allIDs)

	// --- Phase 2: idempotent Set (set same label twice) ---
	for _, id := range allIDs {
		nodeVal := int(id)
		if err := g.SetNodeLabel(nodeVal, "Alpha"); err != nil {
			t.Fatalf("idempotent SetNodeLabel(%d, Alpha): %v", nodeVal, err)
		}
		// Oracle must not change.
	}
	checkOracle(t, "after-idempotent-set", g, oracle, labels, allIDs)

	// --- Phase 3: remove one label from every node ---
	for _, id := range allIDs {
		nodeVal := int(id)
		g.RemoveNodeLabel(nodeVal, "Beta")
		oracle.remove(id, "Beta")
	}
	checkOracle(t, "after-remove-beta", g, oracle, labels, allIDs)

	// --- Phase 4: double Remove is a no-op; no panic ---
	for _, id := range allIDs {
		nodeVal := int(id)
		g.RemoveNodeLabel(nodeVal, "Beta") // already absent
		// Oracle unchanged.
	}
	checkOracle(t, "after-double-remove-beta", g, oracle, labels, allIDs)

	// --- Phase 5: remove remaining labels to leave nodes label-free ---
	for _, id := range allIDs {
		nodeVal := int(id)
		g.RemoveNodeLabel(nodeVal, "Alpha")
		oracle.remove(id, "Alpha")
		g.RemoveNodeLabel(nodeVal, "Gamma")
		oracle.remove(id, "Gamma")
	}
	checkOracle(t, "after-remove-all", g, oracle, labels, allIDs)

	// Verify oracle is empty (all nodes are label-free).
	if len(oracle) != 0 {
		t.Errorf("oracle not empty after removing all labels: %d entries remain", len(oracle))
	}
}

// TestLPG_NodeLabel is the top-level test. Sub-tests for each shape run
// via t.Run; the AllocsPerRun check is a non-parallel sibling to guarantee
// the allocator measurements are not contaminated by concurrent goroutines.
func TestLPG_NodeLabel(t *testing.T) {
	t.Run("Star", func(t *testing.T) {
		t.Parallel()
		exerciseShape(t, shapegen.Star(1024, true))
	})

	t.Run("Complete", func(t *testing.T) {
		t.Parallel()
		exerciseShape(t, shapegen.Complete(128, true))
	})

	t.Run("CompleteBipartite", func(t *testing.T) {
		t.Parallel()
		exerciseShape(t, shapegen.CompleteBipartite(64, 64))
	})

	// AllocsPerRun: HasNodeLabel must be zero-alloc once the label is
	// already interned and the node is present. This sub-test is NOT
	// parallel so testing.AllocsPerRun is meaningful.
	t.Run("HasNodeLabel_ZeroAlloc", func(t *testing.T) {
		g := lpg.New[int, int64](adjlist.Config{Directed: true})
		if err := g.SetNodeLabel(0, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		allocs := testing.AllocsPerRun(100, func() {
			_ = g.HasNodeLabel(0, "Person")
		})
		if allocs != 0 {
			t.Errorf("HasNodeLabel allocs = %v, want 0", allocs)
		}
	})

	// AllocsPerRun: HasNodeLabel on an absent label must also be
	// zero-alloc (both registry fast-path and shard fast-path return
	// early without allocating).
	t.Run("HasNodeLabel_ZeroAlloc_AbsentLabel", func(t *testing.T) {
		g := lpg.New[int, int64](adjlist.Config{Directed: true})
		if err := g.SetNodeLabel(0, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		// Intern "Ghost" once so the registry lookup itself is not the
		// allocation source.
		if err := g.SetNodeLabel(1, "Ghost"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		g.RemoveNodeLabel(1, "Ghost")
		allocs := testing.AllocsPerRun(100, func() {
			_ = g.HasNodeLabel(0, "Ghost")
		})
		if allocs != 0 {
			t.Errorf("HasNodeLabel (absent) allocs = %v, want 0", allocs)
		}
	})
}

// Compile-time check: shapegen.Shape[int, int64] is the type returned by each
// constructor. This is a static assertion only; no runtime cost.
var _ shapegen.Shape[int, int64] = shapegen.Star(1, true)
var _ shapegen.Shape[int, int64] = shapegen.Complete(2, true)
var _ shapegen.Shape[int, int64] = shapegen.CompleteBipartite(1, 1)
