package main

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// openScaledSeeded opens a fresh store and seeds the fixture grown by the given
// synthetic scale, so the multi-label cardinality skew is pronounced. It fails
// the test on any error and registers Close as cleanup.
func openScaledSeeded(t *testing.T, scale synthScale) *dataStore {
	t.Helper()
	ds, err := openStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	seeded, err := seedFixtureScaled(ds.txnStore, scale)
	if err != nil {
		t.Fatalf("seedFixtureScaled: %v", err)
	}
	if !seeded {
		t.Fatal("seedFixtureScaled: want seeded=true on a fresh store")
	}
	return ds
}

// TestMinLabelScanDemo_AnchorsOnSmallerLabel proves the planner re-anchors the
// demo query's multi-label scan onto the smaller :Repository type label rather
// than scanning the whole :Code layer written first. It is the deterministic
// core of the reportMinLabelScan startup telemetry, asserted without any timing.
func TestMinLabelScanDemo_AnchorsOnSmallerLabel(t *testing.T) {
	// A modest component scale makes the Code layer dominate its Repository roots
	// (|Code| ≫ |Repository|) so the re-anchor is a clear, meaningful choice.
	ds := openScaledSeeded(t, synthScale{components: 2000, seed: 1})

	demo, err := collectMinLabelScanDemo(ds.graph)
	if err != nil {
		t.Fatalf("collectMinLabelScanDemo: %v", err)
	}

	if demo.anchorLabel != mlsTypeLabel {
		t.Errorf("scan anchored on %q, want the smaller label %q\nplan:\n%s",
			demo.anchorLabel, mlsTypeLabel, demo.plan)
	}
	if !demo.anchoredOnSmallerLabel() {
		t.Errorf("anchoredOnSmallerLabel()=false; anchor=%q layer=%d type=%d",
			demo.anchorLabel, demo.layerCount, demo.typeCount)
	}
	if demo.typeCount >= demo.layerCount {
		t.Errorf("expected a cardinality skew: type_count=%d must be < layer_count=%d",
			demo.typeCount, demo.layerCount)
	}
	if !strings.Contains(demo.plan, "NodeByLabelScan ["+"r:"+mlsTypeLabel+"]") {
		t.Errorf("plan does not scan the smaller label %q:\n%s", mlsTypeLabel, demo.plan)
	}
	if strings.Contains(demo.plan, "NodeByLabelScan ["+"r:"+mlsLayerLabel+"]") {
		t.Errorf("plan must NOT scan the broad layer label %q:\n%s", mlsLayerLabel, demo.plan)
	}
	// Every :Repository node is also :Code, so the conjunction :Code ∩ :Repository
	// equals the :Repository set: the demo returns exactly the repository roots.
	if int64(demo.rows) != demo.typeCount {
		t.Errorf("result rows = %d, want %d (all Repository nodes are Code)", demo.rows, demo.typeCount)
	}
}

// TestMinLabelScanDemo_ResultIdentity confirms the demo query returns the
// identical row count with the optimisation ON (default) and OFF
// (DisableMinLabelScan), so the contrast reportMinLabelScan prints compares like
// with like — the re-anchor is a physical substitution, never a semantic change.
func TestMinLabelScanDemo_ResultIdentity(t *testing.T) {
	ds := openScaledSeeded(t, synthScale{components: 500, seed: 1})
	ctx := context.Background()

	on := cypher.NewEngineWithOptions(ds.graph, cypher.EngineOptions{})
	off := cypher.NewEngineWithOptions(ds.graph, cypher.EngineOptions{DisableMinLabelScan: true})

	non, err := mlsDrainCount(ctx, on, minLabelDemoQuery)
	if err != nil {
		t.Fatalf("drain ON: %v", err)
	}
	noff, err := mlsDrainCount(ctx, off, minLabelDemoQuery)
	if err != nil {
		t.Fatalf("drain OFF: %v", err)
	}
	if non != noff {
		t.Fatalf("row-count mismatch: on=%d off=%d", non, noff)
	}
	if non == 0 {
		t.Fatal("demo query returned 0 rows; expected at least the repository roots")
	}
}

// TestScanAnchorLabel exercises the plan-parsing helper directly, including the
// no-scan case (an index seek leaves no NodeByLabelScan leaf).
func TestScanAnchorLabel(t *testing.T) {
	cases := []struct {
		name string
		plan string
		want string
	}{
		{"re-anchored", "Projection\n└─ Selection\n   └─ NodeByLabelScan [r:Repository]\n", "Repository"},
		{"single label", "NodeByLabelScan [n:Component]\n", "Component"},
		{"index seek — no scan leaf", "Projection\n└─ NodeByIndexSeek\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scanAnchorLabel(c.plan); got != c.want {
				t.Errorf("scanAnchorLabel(%q) = %q, want %q", c.plan, got, c.want)
			}
		})
	}
}
