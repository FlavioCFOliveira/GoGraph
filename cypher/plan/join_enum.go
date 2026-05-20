package plan

// JoinNode represents one pattern in a multi-pattern MATCH. The planner
// enumerates these into a left-deep Expand/Apply chain.
type JoinNode struct {
	NodeVar  string
	LabelID  uint32
	EqPropID uint32 // 0 = no eq predicate
}

// LeftDeepPlan is the output of greedy join enumeration: an ordered list
// of JoinNodes from cheapest (leftmost) to most expensive, representing a
// left-deep join tree.
type LeftDeepPlan struct {
	Order []JoinNode
	// Costs records the per-step estimated intermediate row count (for EXPLAIN).
	Costs []float64
}

// EnumerateLeftDeep produces a greedy left-deep join order for patterns.
// The algorithm:
//  1. For each JoinNode, compute its leaf scan cost via SelectScanStrategy.
//  2. Choose the cheapest leaf as the first node.
//  3. Iteratively extend: for each remaining node, estimate the intermediate
//     cardinality as current_rows * selectivity(node) and pick the minimum.
//  4. Repeat until all nodes are placed.
//
// O(n²) for n patterns; sufficient for n ≤ 8 per CLAUDE.md.
func EnumerateLeftDeep(patterns []JoinNode, est Estimator, reg *IndexRegistry) LeftDeepPlan {
	n := len(patterns)
	if n == 0 {
		return LeftDeepPlan{}
	}

	// remaining tracks which patterns have not yet been placed.
	remaining := make([]bool, n)
	for i := range remaining {
		remaining[i] = true
	}

	order := make([]JoinNode, 0, n)
	costs := make([]float64, 0, n)

	// Total node count for selectivity denominator.
	totalNodes := float64(est.LabelCount(0))
	if totalNodes < 1 {
		totalNodes = 1
	}

	// Step 1 & 2: pick the cheapest leaf as the first node.
	firstIdx := -1
	firstCost := -1.0
	for i, p := range patterns {
		d := SelectScanStrategy(ScanInput{
			LabelID:     p.LabelID,
			EqPropID:    p.EqPropID,
			RangePropID: 0,
		}, est, reg)
		if firstIdx < 0 || d.Cost < firstCost {
			firstIdx = i
			firstCost = d.Cost
		}
	}

	remaining[firstIdx] = false
	order = append(order, patterns[firstIdx])
	costs = append(costs, firstCost)
	currentRows := firstCost

	// Steps 3 & 4: greedily extend.
	for placed := 1; placed < n; placed++ {
		bestIdx := -1
		bestCost := -1.0

		for i, p := range patterns {
			if !remaining[i] {
				continue
			}
			// Selectivity of this node relative to the total graph.
			labelRows := float64(est.LabelCount(p.LabelID))
			if labelRows < 1 {
				labelRows = 1
			}
			selectivity := labelRows / totalNodes
			intermediate := currentRows * selectivity

			if bestIdx < 0 || intermediate < bestCost {
				bestIdx = i
				bestCost = intermediate
			}
		}

		remaining[bestIdx] = false
		order = append(order, patterns[bestIdx])
		costs = append(costs, bestCost)
		currentRows = bestCost
	}

	return LeftDeepPlan{Order: order, Costs: costs}
}
