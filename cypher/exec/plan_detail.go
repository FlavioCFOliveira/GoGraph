package exec

// This file gives the operators that took a visible physical decision their
// [PlanDetail] string, so a rendered plan states not just WHICH operator ran but
// what it ran against (rmp #2222 AC 1: the chosen access path).
//
// The operator's identity already comes from its concrete type name — a seek is
// a NodeByIndexSeek because it IS one — so PlanDetail carries only what the name
// cannot: the label scanned, the bound sought, the join's build side. Anything
// derivable from the type name is deliberately left out.
//
// PlanDetail is optional: an operator without one renders as its name alone, so
// no operator is obliged to implement it and none is misrepresented by the
// absence.

// PlanDetail reports the label this scan iterates.
func (op *NodeByLabelScan) PlanDetail() string { return op.label }

// PlanDetail reports the value this seek looks up, which is the whole point of
// the access path: an index seek is only as selective as its key.
func (op *NodeByIndexSeek) PlanDetail() string {
	if op.seek == nil {
		return ""
	}
	return "seek=" + op.seek.String()
}

// PlanDetail reports the bounds of the range this scan walks, so a reader can
// see whether the seek is a point lookup, a half-open range, or effectively a
// full index walk.
func (op *NodeByIndexRangeScan) PlanDetail() string {
	return "range=" + boundText(op.lo, "-inf") + ".." + boundText(op.hi, "+inf")
}

// boundText renders one range bound, using unbounded for a nil value.
func boundText(b RangeBound, unbounded string) string {
	if b.Value == nil {
		return unbounded
	}
	s := b.Value.String()
	if !b.Include {
		return s + "(excl)"
	}
	return s
}

// PlanDetail reports which side of the join was materialised into the hash
// table. The build side is the operator's cost centre and the reason one input
// order beats the other, and it is not visible from the operator name.
func (op *HashJoin) PlanDetail() string { return "build=" + buildSideText(op.buildOnLeft) }

// PlanDetail reports the build side of the columnar join, as [HashJoin.PlanDetail]
// does for the row-mode one.
func (op *ColumnarHashJoin) PlanDetail() string {
	return "build=" + buildSideText(op.buildOnLeft)
}

// buildSideText names the join input that is materialised.
func buildSideText(onLeft bool) string {
	if onLeft {
		return "left"
	}
	return "right"
}

// The parallel tier deliberately has no PlanDetail. Its engagement is already
// stated by the operator name — ParallelCountScan, ParallelScanProject and
// ParallelAggregateScan are distinct types from their serial counterparts, which
// is what rmp #2222 AC 1 asks the surface to show. The worker count is not a
// property of the operator: it is negotiated at run time with a shared
// ParallelGovernor, so there is no fixed number to report and inventing one would
// be worse than saying nothing.
