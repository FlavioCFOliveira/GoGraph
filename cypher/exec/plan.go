package exec

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// PlanChildren is implemented by every operator that draws rows from other
// operators. An operator that does not implement it is rendered as a leaf.
//
// It exists because an operator's inputs are held in UNEXPORTED fields, which
// reflection cannot read: `reflect.Value.Interface` panics on an unexported
// field, so the plan tree cannot be recovered by inspection alone. The method is
// therefore the structural contract, and a source-level completeness gate
// (TestPlanChildren_EveryOperatorWithInputsImplementsIt) fails the build if an
// operator that holds an input forgets it — otherwise a rendered plan would
// silently truncate at that node, which is the class of defect this whole
// surface exists to remove.
//
// Return the inputs in EXECUTION order, which for an asymmetric operator is the
// order that explains the cost: a join returns its build side before its probe
// side, an apply its outer before its inner.
type PlanChildren interface {
	PlanChildren() []Operator
}

// PlanDetail is implemented by operators that took a physical decision worth
// showing next to their name — the label they scan, the index they seek, the
// tier they engaged. It is optional: an operator without it renders as its name
// alone.
//
// Keep the string short and factual; it is appended in square brackets after the
// operator name.
type PlanDetail interface {
	PlanDetail() string
}

// PlanNode is one operator in a rendered physical plan.
//
// Name is the operator's CONCRETE Go type name, taken from the value itself, so
// it cannot disagree with the operator that runs. That is the property the plan
// surface turns on: a HashJoin substituted for a nested loop is named HashJoin
// because it IS a *HashJoin, not because a second reconstruction of the planner's
// decisions happened to agree (rmp #2222).
type PlanNode struct {
	// Name is the concrete operator type name, e.g. "HashJoin".
	Name string
	// Detail is the operator's own [PlanDetail], empty when it has none.
	Detail string
	// Children are the operator's inputs, in execution order.
	Children []PlanNode

	// Rows is the number of rows this operator emitted, and Time the wall-clock
	// time attributed to its own Next calls. Both are zero unless the plan was
	// captured by a profiling run ([Profiler]); Profiled records which.
	Rows     int64
	Time     time.Duration
	Profiled bool

	// DbHits is the number of logical storage record accesses attributed to this
	// operator — the measure that distinguishes a selective seek from a scan that
	// filtered afterwards, since both can emit the same few rows while touching
	// wildly different amounts of storage (rmp #2238).
	//
	// It is counted only where it can be counted EXACTLY: at the operators that
	// read records from storage, one hit per record read (see [StorageRecordScan]).
	// An operator that only transforms rows its children produced reports 0,
	// because it accessed no storage. That convention is the one the in-tree
	// db-hits work established (T910/T913) and it is a documented DIVERGENCE from
	// Neo4j, which additionally charges a hit per property read; see docs/cypher.md.
	DbHits int64
}

// PlanTree builds the physical plan tree rooted at op.
//
// It follows [PlanChildren] for structure and reads each node's name from its
// concrete type. When op is a profiling wrapper the wrapper is transparent: the
// node carries the wrapped operator's name with the wrapper's measurements.
func PlanTree(op Operator) PlanNode {
	if op == nil {
		return PlanNode{Name: "(empty)"}
	}

	n := PlanNode{}
	inner := op
	if p, ok := op.(profiledNode); ok {
		// Attribute the measurements to the operator that did the work, and name
		// the node after it rather than after the wrapper.
		inner = p.planUnwrap()
		n.Rows, n.Time, n.DbHits = p.planStats()
		n.Profiled = true
	}

	n.Name = operatorName(inner)
	if d, ok := inner.(PlanDetail); ok {
		n.Detail = d.PlanDetail()
	}
	if kids, ok := inner.(PlanChildren); ok {
		for _, c := range kids.PlanChildren() {
			if c == nil {
				continue
			}
			n.Children = append(n.Children, PlanTree(c))
		}
	}
	return n
}

// operatorName returns the concrete type name of op, without its package
// qualifier or pointer marker.
func operatorName(op Operator) string {
	t := reflect.TypeOf(op)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return "UnknownOperator"
	}
	return t.Name()
}

// RenderPlan renders the physical plan rooted at op as an indented tree, in the
// same shape the logical-plan renderer uses so the two read alike:
//
//	ProduceResults
//	└─ EagerAggregation
//	   └─ HashJoin [build=a, probe=b]
//	      ├─ NodeByLabelScan [a:P]
//	      └─ NodeByLabelScan [b:P]
//
// A profiled plan appends each operator's emitted rows and self time.
func RenderPlan(op Operator) string {
	tree := PlanTree(op)
	return RenderPlanNode(&tree)
}

// RenderPlanNode renders an already-captured tree, so a caller that kept a
// [PlanNode] (for example from a profiling run whose operators are closed) can
// still print it. It does not modify n.
//
// When any node in the tree carries measurements, the ones that do not are
// labelled "(not measured)" rather than left bare. That distinction is load
// bearing: a bare node in a profiled plan would read as an operator that cost
// nothing, when in fact it was never instrumented.
//
// Such nodes USED to exist: instrumentation is applied at one point, the value the
// recursive builder returns, and a composite lowering emits several operators for a
// single logical node, of which only the outermost passed through it. rmp #2237
// closed that by instrumenting each composite site, and
// TestProfile_EveryOperatorIsMeasured holds it closed. The label is kept because
// naming an unmeasured node honestly is still preferable to hiding it or inventing
// a zero, and a future composite lowering could reopen the gap.
func RenderPlanNode(n *PlanNode) string {
	var b strings.Builder
	writePlanNode(&b, n, "", "", anyProfiled(n))
	return strings.TrimRight(b.String(), "\n")
}

// anyProfiled reports whether any node in the tree carries measurements.
func anyProfiled(n *PlanNode) bool {
	if n.Profiled {
		return true
	}
	for i := range n.Children {
		if anyProfiled(&n.Children[i]) {
			return true
		}
	}
	return false
}

// writePlanNode writes n and its subtree. prefix is the text printed before this
// node's connector; childPrefix is what the node's descendants inherit.
func writePlanNode(b *strings.Builder, n *PlanNode, prefix, childPrefix string, anyMeasured bool) {
	b.WriteString(prefix)
	b.WriteString(n.Name)
	if n.Detail != "" {
		b.WriteString(" [")
		b.WriteString(n.Detail)
		b.WriteString("]")
	}
	switch {
	case n.Profiled:
		fmt.Fprintf(b, " (rows=%d, dbhits=%d, time=%s)", n.Rows, n.DbHits, n.Time.Round(time.Microsecond))
	case anyMeasured:
		b.WriteString(" (not measured)")
	}
	b.WriteString("\n")

	for i := range n.Children {
		last := i == len(n.Children)-1
		branch, cont := "├─ ", "│  "
		if last {
			branch, cont = "└─ ", "   "
		}
		writePlanNode(b, &n.Children[i], childPrefix+branch, childPrefix+cont, anyMeasured)
	}
}
