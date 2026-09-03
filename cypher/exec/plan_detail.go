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

import "strings"

// PlanDetail reports the label this scan iterates, or — for the multi-label
// conjunction form (#2133) — the intersected labels in the order they are ANDed,
// which is the order the planner chose by ascending cardinality and is therefore
// part of the physical decision a reader needs to see.
func (op *NodeByLabelScan) PlanDetail() string {
	if op.labels != nil {
		return strings.Join(op.labels, "∩")
	}
	return op.label
}

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
	d := "range=" + boundText(op.lo, "-inf") + ".." + boundText(op.hi, "+inf")
	// A composed intersection (#2134) probes several indexes and ANDs them, so the
	// primary range alone would misrepresent the access path: the reader needs to
	// see that more than one index contributed and over which intervals.
	for i := range op.extra {
		d += " ∩ range=" + boundText(op.extra[i].Lo, "-inf") + ".." + boundText(op.extra[i].Hi, "+inf")
	}
	return d
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

// PlanDetail reports whether this hop's destination is already bound and, when it
// is, which access path it takes to reach it (#2149). This is exactly the kind of
// physical decision the type name cannot carry: a bound-destination hop is still
// an *Expand, but it either SEEKS the destination's contiguous run in the
// destination-ordered CSR — O(log d + r) — or walks the whole neighbour run and
// filters, which is Θ(d). The two differ by an asymptotic factor on the shape
// behind triangles, cycle closing and mutual-relationship detection, so a plan
// that did not distinguish them would hide the change this operator exists to make.
//
// "ExpandInto" is the name openCypher implementations conventionally give this
// access path; it appears in the DETAIL rather than as the operator name because a
// rendered name is the concrete Go type and must stay incapable of disagreeing with
// the operator that runs (rmp #2222). An ordinary hop returns "" and renders as
// "Expand" alone.
func (op *Expand) PlanDetail() string {
	if op.intoCol < 0 {
		return ""
	}
	if op.intoSeek {
		return "ExpandInto seek"
	}
	return "ExpandInto filter"
}

// PlanDetail names the morsel-parallel tier and states plainly that this leaf's
// db-hits are not counted.
//
// The tier is a physical decision a reader needs to see — the same class as the
// label a scan iterates — and without it a parallel leaf renders bare, so nothing
// tells a reader that the operator's whole sub-plan collapsed into one line.
//
// The db-hits clause is here because the alternative would be worse. A
// morsel-parallel leaf reads one node reference per node its workers walk, and
// implements neither [StorageRecordScan] nor storageAccessCounter, so its DbHits
// cell reads 0 for a full scan of the graph. Zero is also what a pure row
// transformer reports, and the column has no way to tell "counted, and it is
// zero" from "not counted at all" — Neo4j's renderer leaves such a cell BLANK and
// prints "x + ?" for a total it could not complete
// (renderAsTreeTable.scala / renderSummary.scala, 5.26.16), and PostgreSQL
// suppresses an unmeasured figure rather than printing 0 (explain.c, REL_17).
// GoGraph's column prints the zero, so the qualification is put where the reader
// will see it next to the number (rmp #2720).
//
// Marking the leaf [StorageRecordScan] instead was rejected: its emitted row
// count is NOT its node-walk count whenever the fused sub-plan carries a
// Selection, which is exactly the min-label shape the parallel scan is built for,
// so the marker would replace an obvious zero with a plausible wrong number.
func (op *ParallelScanProject) PlanDetail() string { return parallelPlanDetail }

// PlanDetail names the tier and its db-hits gap, as [ParallelScanProject.PlanDetail]
// does. An aggregate leaf emits one row per group while walking the whole label,
// so for it the row count is not even close to the node-walk count.
func (op *ParallelAggregateScan) PlanDetail() string { return parallelPlanDetail }

// PlanDetail names the tier and its db-hits gap, as [ParallelScanProject.PlanDetail]
// does. A count leaf emits exactly one row however many nodes it walked.
func (op *ParallelCountScan) PlanDetail() string { return parallelPlanDetail }

// parallelPlanDetail is the shared detail string of the morsel-parallel leaves, so
// the three cannot drift apart in what they tell a reader.
const parallelPlanDetail = "parallel tier; db-hits not counted"

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
