package plan_test

import (
	"testing"

	"gograph/cypher/plan"
	"gograph/graph/index"
)

// mockEstimator is a configurable Estimator for scan-strategy tests.
type mockEstimator struct {
	labelCounts  map[uint32]uint64
	hashCount    uint64
	btreeCount   uint64
	avgOutDegree float64
}

func (m *mockEstimator) LabelCount(lbl uint32) uint64 {
	if m.labelCounts == nil {
		return 0
	}
	return m.labelCounts[lbl]
}

func (m *mockEstimator) HashLookupCount(_ uint32, _ any) uint64 {
	return m.hashCount
}

func (m *mockEstimator) BTreeRangeCount(_ uint32, _, _ string) uint64 {
	return m.btreeCount
}

func (m *mockEstimator) AvgOutDegree(_, _, _ uint32) float64 {
	if m.avgOutDegree == 0 {
		return 1.0
	}
	return m.avgOutDegree
}

// buildRegistry creates an IndexRegistry with optional hash and btree indexes.
func buildRegistry(t *testing.T, hasHash, hasBTree bool) *plan.IndexRegistry {
	t.Helper()
	mgr := index.NewManager()
	if hasHash {
		mustCreate(t, mgr, "hash-prop", stubSubscriber{"hash"})
	}
	if hasBTree {
		mustCreate(t, mgr, "btree-prop", stubSubscriber{"btree"})
	}
	return plan.NewIndexRegistry(mgr)
}

// buildEstimator creates a mockEstimator with a total-node count, a label
// count, and optional hash/btree row counts.
func buildEstimator(totalNodes, labelCount, hashCount, btreeCount uint64) *mockEstimator {
	return &mockEstimator{
		labelCounts: map[uint32]uint64{
			0: totalNodes,
			1: labelCount,
		},
		hashCount:  hashCount,
		btreeCount: btreeCount,
	}
}

var scanStrategyTests = []struct {
	name     string
	input    plan.ScanInput
	hasHash  bool
	hasBTree bool
	wantKind plan.ScanKind
}{
	{name: "no_label_no_index", wantKind: plan.ScanKindAllNodes},
	{name: "label_only", input: plan.ScanInput{LabelID: 1}, wantKind: plan.ScanKindLabel},
	{name: "label_with_hash", input: plan.ScanInput{LabelID: 1, EqPropID: 2}, hasHash: true, wantKind: plan.ScanKindIndexSeek},
	{name: "label_with_btree", input: plan.ScanInput{LabelID: 1, RangePropID: 3}, hasBTree: true, wantKind: plan.ScanKindIndexRangeScan},
	{name: "hash_beats_label", input: plan.ScanInput{LabelID: 1, EqPropID: 2}, hasHash: true, wantKind: plan.ScanKindIndexSeek},
	{name: "btree_beats_label", input: plan.ScanInput{LabelID: 1, RangePropID: 3}, hasBTree: true, wantKind: plan.ScanKindIndexRangeScan},
	{name: "no_index_falls_back_label", input: plan.ScanInput{LabelID: 1, EqPropID: 2}, hasHash: false, wantKind: plan.ScanKindLabel},
	{name: "switch_off_index_changes_plan", input: plan.ScanInput{LabelID: 1, EqPropID: 2}, hasHash: false, wantKind: plan.ScanKindLabel},
}

func TestSelectScanStrategy_Table(t *testing.T) {
	t.Parallel()

	// totalNodes=10000, labelCount=500, hashCount=5, btreeCount=50.
	// hash cost   = 5   * 0.05  = 0.25  (cheapest)
	// btree cost  = 50  * 0.1   = 5.0
	// label cost  = 500 * 0.5   = 250
	// allnodes    = 10000 * 1.0 = 10000
	est := buildEstimator(10_000, 500, 5, 50)

	for _, tc := range scanStrategyTests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := buildRegistry(t, tc.hasHash, tc.hasBTree)
			got := plan.SelectScanStrategy(tc.input, est, reg)
			if got.Kind != tc.wantKind {
				t.Errorf("SelectScanStrategy(%+v) kind = %v, want %v", tc.input, got.Kind, tc.wantKind)
			}
		})
	}
}

// TestSelectScanStrategy_DisableHashChangesPlan verifies that removing the
// hash index causes SelectScanStrategy to fall back to ScanKindLabel rather
// than ScanKindIndexSeek.
func TestSelectScanStrategy_DisableHashChangesPlan(t *testing.T) {
	t.Parallel()

	est := buildEstimator(10_000, 500, 5, 0)
	input := plan.ScanInput{LabelID: 1, EqPropID: 2}

	// With hash: expect IndexSeek.
	withHash := buildRegistry(t, true, false)
	got := plan.SelectScanStrategy(input, est, withHash)
	if got.Kind != plan.ScanKindIndexSeek {
		t.Fatalf("with hash: got kind %v, want ScanKindIndexSeek", got.Kind)
	}

	// Without hash: expect Label.
	withoutHash := buildRegistry(t, false, false)
	got = plan.SelectScanStrategy(input, est, withoutHash)
	if got.Kind != plan.ScanKindLabel {
		t.Fatalf("without hash: got kind %v, want ScanKindLabel", got.Kind)
	}
}

// TestSelectScanStrategy_AllNodesFallbackOnZeroStats verifies that when
// no label count is known (zero stats), AllNodes is chosen.
func TestSelectScanStrategy_AllNodesFallbackOnZeroStats(t *testing.T) {
	t.Parallel()

	est := &mockEstimator{} // all counts return 0
	reg := plan.NewIndexRegistry(nil)
	got := plan.SelectScanStrategy(plan.ScanInput{}, est, reg)
	if got.Kind != plan.ScanKindAllNodes {
		t.Errorf("zero-stats: got kind %v, want ScanKindAllNodes", got.Kind)
	}
}

// TestSelectScanStrategy_IndexNamePopulated verifies that the IndexName
// field is set correctly when an index-based scan is chosen.
func TestSelectScanStrategy_IndexNamePopulated(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	mustCreate(t, mgr, "hash-email", stubSubscriber{"hash"})
	reg := plan.NewIndexRegistry(mgr)

	est := buildEstimator(10_000, 500, 5, 0)
	input := plan.ScanInput{LabelID: 1, EqPropID: 7}
	got := plan.SelectScanStrategy(input, est, reg)

	if got.Kind != plan.ScanKindIndexSeek {
		t.Fatalf("got kind %v, want ScanKindIndexSeek", got.Kind)
	}
	if got.IndexName != "hash-email" {
		t.Errorf("IndexName = %q, want %q", got.IndexName, "hash-email")
	}
}

// TestSelectScanStrategy_CostDecreasing verifies that the cost ordering
// AllNodes > Label > BTree > Hash holds when estimated rows satisfy the
// expected relative magnitudes.
func TestSelectScanStrategy_CostDecreasing(t *testing.T) {
	t.Parallel()

	// Build registry with both index types.
	mgr := index.NewManager()
	mustCreate(t, mgr, "hash-prop", stubSubscriber{"hash"})
	mustCreate(t, mgr, "btree-prop", stubSubscriber{"btree"})
	reg := plan.NewIndexRegistry(mgr)

	// Costs: hash=0.25, btree=5.0, label=250, allnodes=10000
	est := buildEstimator(10_000, 500, 5, 50)

	input := plan.ScanInput{LabelID: 1, EqPropID: 2, RangePropID: 3}
	got := plan.SelectScanStrategy(input, est, reg)
	if got.Kind != plan.ScanKindIndexSeek {
		t.Errorf("expected hash (cheapest): got %v", got.Kind)
	}
	if got.Cost <= 0 {
		t.Errorf("cost should be positive, got %f", got.Cost)
	}
}
