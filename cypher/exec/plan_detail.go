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

// PlanDetail names the morsel-parallel tier and states what the leaf's single
// line accounts for.
//
// The tier is a physical decision a reader needs to see — the same class as the
// label a scan iterates — and without it a parallel leaf renders bare, so nothing
// tells a reader that the operator's whole sub-plan collapsed into one line.
//
// # What the wording used to say, and why it changed
//
// Until rmp #2762 this read "parallel tier; db-hits not counted", because the leaf
// implemented neither [StorageRecordScan] nor storageAccessCounter and its DbHits
// cell was a figure nobody had taken. That is no longer true: since #2762 each leaf
// implements storageAccessCounter and reports the node references its workers
// consumed, so the same query reports the same figure on either side of the
// parallel threshold. Leaving the old words in place would have made the plan line
// contradict its own number.
//
// What replaces it is the property that IS still true and still needs saying: the
// whole parallel phase — every worker, every morsel, and the sub-plan each worker
// built — is attributed to this ONE node. Rows, time and db-hits are all totals for
// the phase, and there is no sub-tree below the line to subtract, because a
// morsel-parallel leaf implements no [PlanChildren] and the builder clears the
// profiler from the per-worker build options. A reader who does not know that would
// take the leaf for an ordinary scan and look for the filter and projection that are
// fused inside it. See the "parallel tier" section of exec.Profiler, which also
// records why PostgreSQL's per-worker breakdown is deliberately not followed.
//
// Marking the leaf [StorageRecordScan] instead was rejected then and is still
// wrong: its emitted row count is NOT its node-walk count whenever the fused
// sub-plan carries a Selection, which is exactly the min-label shape the parallel
// scan is built for, so the marker would have replaced an obvious zero with a
// plausible wrong number. The figure had to be counted, and now is.
func (op *ParallelScanProject) PlanDetail() string { return parallelPlanDetail }

// PlanDetail names the tier and its one-node attribution, as
// [ParallelScanProject.PlanDetail] does. This leaf emits one row per GROUP while
// walking the whole node source, so its row count says even less about its storage
// work than the fused scan's does — which is why its db-hits figure is counted
// (rmp #2762) rather than derived from rows.
func (op *ParallelAggregateScan) PlanDetail() string { return parallelPlanDetail }

// PlanDetail names the tier and its one-node attribution, as
// [ParallelScanProject.PlanDetail] does. This leaf emits exactly ONE row however
// many nodes it walked, which is the extreme case of the same point: rows=1,
// dbhits=N.
func (op *ParallelCountScan) PlanDetail() string { return parallelPlanDetail }

// parallelPlanDetail is the shared detail string of the morsel-parallel leaves, so
// the three cannot drift apart in what they tell a reader.
const parallelPlanDetail = "parallel tier; whole phase on one node"

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

// The parallel tier's PlanDetail deliberately does NOT report a worker count.
// Engagement is already stated by the operator name — ParallelCountScan,
// ParallelScanProject and ParallelAggregateScan are distinct types from their
// serial counterparts, which is what rmp #2222 AC 1 asks the surface to show — and
// the worker count is not a property of the operator: it is negotiated at run time
// with a shared ParallelGovernor against the parallel leaves in flight across every
// concurrent query, so there is no fixed number to report and inventing one would
// be worse than saying nothing.
//
// This paragraph used to claim the tier had no PlanDetail at all. It has had one
// since rmp #2720 (the three functions above); the surviving true part is the
// refusal to print a worker count, which is what it now says.
