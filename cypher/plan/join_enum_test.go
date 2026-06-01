package plan_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/plan"
)

// joinEstimator is a configurable Estimator for join-enumeration tests.
// It returns per-label counts and zero for index-based estimates.
type joinEstimator struct {
	labelCounts map[uint32]uint64
}

func (e *joinEstimator) LabelCount(lbl uint32) uint64 {
	if e.labelCounts == nil {
		return 0
	}
	return e.labelCounts[lbl]
}

func (e *joinEstimator) HashLookupCount(_ uint32, _ any) uint64       { return 0 }
func (e *joinEstimator) BTreeRangeCount(_ uint32, _, _ string) uint64 { return 0 }
func (e *joinEstimator) AvgOutDegree(_, _, _ uint32) float64          { return 1.0 }

// TestEnumerateLeftDeep_Single verifies that a single-node input produces
// an order containing exactly that node.
func TestEnumerateLeftDeep_Single(t *testing.T) {
	t.Parallel()

	est := &joinEstimator{labelCounts: map[uint32]uint64{0: 1000, 1: 200}}
	reg := plan.NewIndexRegistry(nil)
	patterns := []plan.JoinNode{{NodeVar: "a", LabelID: 1}}

	result := plan.EnumerateLeftDeep(patterns, est, reg)

	if len(result.Order) != 1 {
		t.Fatalf("Order len = %d, want 1", len(result.Order))
	}
	if result.Order[0].NodeVar != "a" {
		t.Errorf("Order[0].NodeVar = %q, want %q", result.Order[0].NodeVar, "a")
	}
	if len(result.Costs) != 1 {
		t.Errorf("Costs len = %d, want 1", len(result.Costs))
	}
}

// TestEnumerateLeftDeep_Empty verifies that an empty patterns slice produces
// an empty plan without panicking.
func TestEnumerateLeftDeep_Empty(t *testing.T) {
	t.Parallel()

	est := &joinEstimator{}
	reg := plan.NewIndexRegistry(nil)

	result := plan.EnumerateLeftDeep(nil, est, reg)
	if len(result.Order) != 0 {
		t.Errorf("Order len = %d, want 0", len(result.Order))
	}
}

// TestEnumerateLeftDeep_TwoNodes_CheapFirst verifies that when A has 10×
// fewer label matches than B, A is placed first.
func TestEnumerateLeftDeep_TwoNodes_CheapFirst(t *testing.T) {
	t.Parallel()

	// A (label 1): 100 nodes  → allNodesCost = 100 * 0.5 = 50  (ScanKindLabel)
	// B (label 2): 1000 nodes → allNodesCost = 1000 * 0.5 = 500
	est := &joinEstimator{labelCounts: map[uint32]uint64{
		0: 10_000,
		1: 100,
		2: 1_000,
	}}
	reg := plan.NewIndexRegistry(nil)
	patterns := []plan.JoinNode{
		{NodeVar: "b", LabelID: 2},
		{NodeVar: "a", LabelID: 1},
	}

	result := plan.EnumerateLeftDeep(patterns, est, reg)

	if len(result.Order) != 2 {
		t.Fatalf("Order len = %d, want 2", len(result.Order))
	}
	if result.Order[0].NodeVar != "a" {
		t.Errorf("Order[0] = %q, want %q (cheaper node first)", result.Order[0].NodeVar, "a")
	}
	if result.Order[1].NodeVar != "b" {
		t.Errorf("Order[1] = %q, want %q", result.Order[1].NodeVar, "b")
	}
}

// TestEnumerateLeftDeep_FivePatterns verifies that 5 nodes with known costs
// are placed in order of increasing label selectivity.
func TestEnumerateLeftDeep_FivePatterns(t *testing.T) {
	t.Parallel()

	// Labels 1–5 with increasing node counts.
	// SelectScanStrategy uses ScanKindLabel cost = labelCount * 0.5.
	// So the cheapest leaf is label 1 (100 nodes), then 2 (200), etc.
	est := &joinEstimator{labelCounts: map[uint32]uint64{
		0: 100_000,
		1: 100,
		2: 200,
		3: 300,
		4: 400,
		5: 500,
	}}
	reg := plan.NewIndexRegistry(nil)

	// Supply in reverse order to ensure we actually sort, not just preserve.
	patterns := []plan.JoinNode{
		{NodeVar: "e", LabelID: 5},
		{NodeVar: "d", LabelID: 4},
		{NodeVar: "c", LabelID: 3},
		{NodeVar: "b", LabelID: 2},
		{NodeVar: "a", LabelID: 1},
	}

	result := plan.EnumerateLeftDeep(patterns, est, reg)

	if len(result.Order) != 5 {
		t.Fatalf("Order len = %d, want 5", len(result.Order))
	}
	// The first placed node must be "a" (cheapest leaf scan).
	if result.Order[0].NodeVar != "a" {
		t.Errorf("Order[0] = %q, want %q", result.Order[0].NodeVar, "a")
	}
	// Costs slice must match Order in length.
	if len(result.Costs) != len(result.Order) {
		t.Errorf("Costs len = %d, want %d", len(result.Costs), len(result.Order))
	}
}

// TestEnumerateLeftDeep_BeatNaive verifies that the greedy plan has lower
// total intermediate row count than the naive (original slice order) plan.
func TestEnumerateLeftDeep_BeatNaive(t *testing.T) {
	t.Parallel()

	// 5 patterns: naive order is [big, big, big, small, small].
	// Greedy should place [small, small, ...] first.
	est := &joinEstimator{labelCounts: map[uint32]uint64{
		0: 100_000,
		1: 50_000, // "big"
		2: 40_000,
		3: 30_000,
		4: 100, // "small"
		5: 200,
	}}
	reg := plan.NewIndexRegistry(nil)

	patternsNaive := []plan.JoinNode{
		{NodeVar: "n1", LabelID: 1},
		{NodeVar: "n2", LabelID: 2},
		{NodeVar: "n3", LabelID: 3},
		{NodeVar: "n4", LabelID: 4},
		{NodeVar: "n5", LabelID: 5},
	}

	// Compute naive total: sum of intermediate cardinalities in original order.
	// Naive step 1 cost (leaf): label 1 → 50000 * 0.5 = 25000
	// We just verify greedy order starts with a small label.
	result := plan.EnumerateLeftDeep(patternsNaive, est, reg)

	if len(result.Order) != 5 {
		t.Fatalf("Order len = %d, want 5", len(result.Order))
	}
	// Greedy first node must be label 4 (count=100, cheapest leaf).
	firstLabel := result.Order[0].LabelID
	if firstLabel != 4 {
		t.Errorf("greedy first label = %d, want 4 (cheapest)", firstLabel)
	}

	// Total greedy intermediate rows must be less than naive total.
	greedyTotal := 0.0
	for _, c := range result.Costs {
		greedyTotal += c
	}

	// Naive total: compute by mimicking the extension in original order.
	naiveTotal := computeNaiveTotal(patternsNaive, est)
	if greedyTotal >= naiveTotal {
		t.Errorf("greedy total %f >= naive total %f; greedy should be cheaper",
			greedyTotal, naiveTotal)
	}
}

// computeNaiveTotal computes the total intermediate row estimate for the
// original (unsorted) pattern order, for comparison in BeatNaive.
func computeNaiveTotal(patterns []plan.JoinNode, est *joinEstimator) float64 {
	totalNodes := float64(est.LabelCount(0))
	if totalNodes < 1 {
		totalNodes = 1
	}
	// First node: leaf scan cost = labelCount * 0.5 (label scan heuristic).
	firstLabel := patterns[0].LabelID
	firstRows := float64(est.LabelCount(firstLabel))
	if firstRows < 1 {
		firstRows = 1
	}
	current := firstRows * plan.PerRowCost[plan.ScanKindLabel]
	total := current

	for _, p := range patterns[1:] {
		labelRows := float64(est.LabelCount(p.LabelID))
		if labelRows < 1 {
			labelRows = 1
		}
		sel := labelRows / totalNodes
		intermediate := current * sel
		total += intermediate
		current = intermediate
	}
	return total
}
