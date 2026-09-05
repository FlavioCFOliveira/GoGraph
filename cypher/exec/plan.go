package exec

import (
	"fmt"
	"reflect"
	"strconv"
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
	// Unlike Rows and Time it is NOT uniformly a measurement. It is meaningful only
	// when DbHitsKnown is true, and then it comes from one of three places:
	//
	//   - MEASURED, for an operator implementing exec's storageAccessCounter —
	//     today [VarLengthExpand], which reports the relationship slots its BFS
	//     actually read;
	//   - DERIVED from the emitted row count, for an operator marked
	//     [StorageRecordScan], whose contract asserts one record read per row;
	//   - a KNOWN ZERO, for an operator marked exec's noStorageAccess, which opens
	//     no access path at all.
	//
	// It is in every case a count of ACCESS-PATH record reads and never of property
	// reads, which is a documented DIVERGENCE from Neo4j, which additionally charges
	// a hit per property read; see docs/cypher.md.
	DbHits int64

	// DbHitsKnown reports whether DbHits is a figure at all.
	//
	// It is false for an operator that reads storage and claims none of the three
	// markers — [ShortestPath], [AllShortestPaths], the morsel-parallel leaves,
	// the count-store leaves, and every operator holding a caller-supplied
	// expression closure that can reach the graph ([Filter], [Project], [Sort],
	// [Top], [Unwind], the hash joins, [RollUpApply], [ProcedureCallOp]). DbHits
	// is then 0 only because an int64 has to hold something, and NO renderer may
	// print it as a count.
	//
	// The distinction exists because it could not previously be drawn: both a pure
	// projection and a parallel scan of 2000 nodes printed `dbhits=0`, so the
	// column could not be read (rmp #2720 §1, rmp #2760). It is the same rule this
	// codebase already applies to the Bolt page-cache fields, which are OMITTED
	// rather than sent as 0 because GoGraph has no page cache and a 0 would be a
	// measurement claim (bolt/server/plan_meta.go).
	//
	// Both incumbents draw it too, read in source: Neo4j carries the sentinel
	// OperatorProfile.NO_DATA = -1 (OperatorProfile.java:59, 5.26.16), drops the
	// argument entirely when it holds that value (PlanDescriptionBuilder.scala,
	// BuildPlanDescription.addArgument), leaves the cell blank, and renders an
	// incomplete total as "x + ?" (renderSummary.scala, TotalHits); PostgreSQL
	// suppresses each zero-valued counter individually under the comment
	// "Show only positive counter values." (explain.c:3764, REL_17_STABLE).
	//
	// A node that was never instrumented at all (Profiled false) also has this
	// false: nothing counted it either.
	DbHitsKnown bool
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
		n.Rows, n.Time, n.DbHits, n.DbHitsKnown = p.planStats()
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
// The same instinct now governs the db-hits figure inside the parenthesis: a
// measured node whose accesses nobody counted renders `dbhits=?`
// ([DbHitsUnknown]) rather than `dbhits=0`, which would read as an operator that
// touched no storage (rmp #2760). [PlanNode.DbHitsKnown] carries the state.
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
		fmt.Fprintf(b, " (rows=%d, dbhits=%s, time=%s)",
			n.Rows, DbHitsCell(n.DbHits, n.DbHitsKnown), n.Time.Round(time.Microsecond))
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

// ─────────────────────────────────────────────────────────────────────────────
// Rendering a db-hits figure that may not exist
// ─────────────────────────────────────────────────────────────────────────────

// DbHitsUnknown is what every renderer prints in place of a db-hits figure
// nobody counted ([PlanNode.DbHitsKnown] false).
//
// It is Neo4j's glyph for the same state, read in source at 5.26.16:
// renderSummary.scala prints "?" for a total whose contributing operators did not
// all report, and renderAsTreeTable.scala simply omits the cell for a plan that
// carries no DbHits argument. Printing "0" instead — which is what GoGraph did
// until rmp #2760 — makes an uncounted operator indistinguishable from one that
// genuinely read nothing.
const DbHitsUnknown = "?"

// DbHitsCell renders one operator's db-hits cell: the number when it is a figure,
// [DbHitsUnknown] when it is not.
//
// Every surface goes through this one function — the indented tree here,
// cypher/explain's columnar table, and the cypher package's two Engine methods —
// so no renderer can print a zero for an uncounted operator by forgetting the
// flag.
func DbHitsCell(hits int64, known bool) string {
	if !known {
		return DbHitsUnknown
	}
	return strconv.FormatInt(hits, 10)
}

// DbHitsTotalCell renders a plan-wide db-hits total that may have summed over
// operators which reported nothing.
//
// total is the sum of the KNOWN cells only; uncertain reports whether any cell
// was excluded from it. The four cases are Neo4j's, transcribed from
// renderSummary.scala's `dbhits` (5.26.16) rather than invented here, because the
// question and the answer are the same:
//
//	TotalHits(0, false) -> "0"        nothing was read, and that is known
//	TotalHits(0, true)  -> "?"        nothing countable was read, and something was not counted
//	TotalHits(x, false) -> "x"        a complete total
//	TotalHits(x, true)  -> "x + ?"    at least x, plus an unknown amount
//
// The "x + ?" form is the load-bearing one: it neither hides the figure the
// engine does have nor lets it be mistaken for the whole query's cost.
func DbHitsTotalCell(total int64, uncertain bool) string {
	n := strconv.FormatInt(total, 10)
	switch {
	case !uncertain:
		return n
	case total == 0:
		return DbHitsUnknown
	default:
		return n + " + " + DbHitsUnknown
	}
}
