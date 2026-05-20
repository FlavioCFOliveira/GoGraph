package plan

// ExpandDirection is the chosen traversal direction.
type ExpandDirection uint8

// Recognised ExpandDirection values. When the two frontier estimates are equal,
// ExpandOut is preferred (stable tie-break).
const (
	ExpandOut ExpandDirection = iota // expand from src → OUT neighbours
	ExpandIn                         // expand from dst → IN neighbours (reverse scan)
)

// ExpandDecision records the result of SelectExpandDirection.
type ExpandDecision struct {
	Dir               ExpandDirection
	EstimatedFrontier float64 // estimated intermediate rows
}

// SelectExpandDirection chooses the cheaper traversal direction for an
// undirected (or undirected-by-preference) relationship. When the dst-side
// label is more selective than the src-side, expand IN; otherwise expand OUT.
//
// srcLabelID and dstLabelID may be 0 (no label constraint). relTypeID is the
// interned relationship type ID; 0 means any type.
func SelectExpandDirection(srcLabelID, relTypeID, dstLabelID uint32, est Estimator) ExpandDecision {
	srcCount := uint64(1)
	if c := est.LabelCount(srcLabelID); c > 0 {
		srcCount = c
	}
	dstCount := uint64(1)
	if c := est.LabelCount(dstLabelID); c > 0 {
		dstCount = c
	}

	degOut := est.AvgOutDegree(srcLabelID, relTypeID, dstLabelID)
	if degOut < 1.0 {
		degOut = 1.0
	}
	// Symmetric estimate: from dst side, treat it as OUT toward src.
	degIn := est.AvgOutDegree(dstLabelID, relTypeID, srcLabelID)
	if degIn < 1.0 {
		degIn = 1.0
	}

	frontierOut := float64(srcCount) * degOut
	frontierIn := float64(dstCount) * degIn

	if frontierIn < frontierOut {
		return ExpandDecision{Dir: ExpandIn, EstimatedFrontier: frontierIn}
	}
	return ExpandDecision{Dir: ExpandOut, EstimatedFrontier: frontierOut}
}
