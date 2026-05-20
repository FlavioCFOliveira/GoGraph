package plan

// ScanKind describes the chosen physical scan type.
type ScanKind uint8

// Recognised ScanKind values ordered from most expensive (AllNodes) to cheapest
// (IndexSeek). The planner always selects the minimum-cost option given the
// available label constraint, equality predicate, and range predicate.
const (
	ScanKindAllNodes       ScanKind = iota // AllNodesScan — no label or index
	ScanKindLabel                          // NodeByLabelScan — label bitmap
	ScanKindIndexSeek                      // NodeByIndexSeek — hash exact-match
	ScanKindIndexRangeScan                 // NodeByIndexRangeScan — btree range
)

// ScanDecision records the outcome of SelectScanStrategy.
type ScanDecision struct {
	Kind      ScanKind
	IndexName string  // non-empty for IndexSeek and IndexRangeScan
	Cost      float64 // estimated rows × per-row cost
}

// ScanInput describes the predicate context for scan selection.
type ScanInput struct {
	Label   string // may be empty (no label constraint)
	LabelID uint32
	// EqPropID is the property key for an equality predicate (prop == value).
	// Zero when no equality predicate is available.
	EqPropID uint32
	// RangePropID is the property key for a range predicate.
	// Zero when no range predicate is available.
	RangePropID uint32
}

// PerRowCost is the assumed relative cost per row for each scan type.
// These are heuristic weights; they should be tuned in production via benchmarks.
var PerRowCost = map[ScanKind]float64{
	ScanKindAllNodes:       1.0,
	ScanKindLabel:          0.5,
	ScanKindIndexSeek:      0.05,
	ScanKindIndexRangeScan: 0.1,
}

// fallbackAllNodeCount is used as the row estimate when LabelCount(0)
// returns 0 (no statistics available). A large value ensures that any
// scan with a real estimate will beat the default AllNodes plan.
const fallbackAllNodeCount = 1e9

// SelectScanStrategy returns the cheapest scan strategy for the given input
// using est for cardinality and reg for available indexes.
// It never returns an error; when information is insufficient, it falls back
// to ScanKindAllNodes.
func SelectScanStrategy(input ScanInput, est Estimator, reg *IndexRegistry) ScanDecision {
	// Step 1: baseline — AllNodes.
	total := float64(est.LabelCount(0))
	if total == 0 {
		total = fallbackAllNodeCount
	}
	best := ScanDecision{
		Kind: ScanKindAllNodes,
		Cost: total * PerRowCost[ScanKindAllNodes],
	}

	// Step 2: label scan.
	if input.LabelID > 0 {
		labelRows := float64(est.LabelCount(input.LabelID))
		if labelRows == 0 {
			labelRows = 1
		}
		labelCost := labelRows * PerRowCost[ScanKindLabel]
		if labelCost < best.Cost {
			best = ScanDecision{
				Kind: ScanKindLabel,
				Cost: labelCost,
			}
		}
	}

	// Step 3: hash index seek (equality predicate).
	if input.EqPropID > 0 && reg.HasHash() {
		hashRows := float64(est.HashLookupCount(input.EqPropID, nil))
		if hashRows == 0 {
			hashRows = 1
		}
		hashCost := hashRows * PerRowCost[ScanKindIndexSeek]
		if hashCost < best.Cost {
			entries := reg.ByKind(IndexKindHash)
			if len(entries) > 0 {
				best = ScanDecision{
					Kind:      ScanKindIndexSeek,
					IndexName: entries[0].Name,
					Cost:      hashCost,
				}
			}
		}
	}

	// Step 4: btree range scan.
	if input.RangePropID > 0 && reg.HasBTree() {
		rangeRows := float64(est.BTreeRangeCount(input.RangePropID, "", ""))
		if rangeRows == 0 {
			rangeRows = 1
		}
		rangeCost := rangeRows * PerRowCost[ScanKindIndexRangeScan]
		if rangeCost < best.Cost {
			entries := reg.ByKind(IndexKindBTree)
			if len(entries) > 0 {
				best = ScanDecision{
					Kind:      ScanKindIndexRangeScan,
					IndexName: entries[0].Name,
					Cost:      rangeCost,
				}
			}
		}
	}

	return best
}
