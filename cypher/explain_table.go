package cypher

// explain_table.go — the COLUMNAR plan renderings (rmp #2701).
//
// GoGraph renders plans as indented trees: [Engine.Explain] walks the physical
// operator tree, [Engine.ExplainLogical] the logical one, and [Engine.Profile]
// the measured physical one. Neo4j renders the same information as a fixed-width
// table, which is what makes two operators' estimates or costs comparable at a
// glance — a column of right-aligned numbers, not numbers scattered along lines
// of varying indentation.
//
// The cypher/explain package already had that renderer and nothing called it. It
// was written before any engine surface existed to wire it to (see the note in
// cypher/exec/profile.go); that condition no longer holds, and this file is the
// wiring: [Engine.ExplainTable] and [Engine.ProfileTable].
//
// # One walk, two renderings
//
// The logical table does NOT re-derive the plan. [explainWithIndexesNode] — the
// walk behind [Engine.ExplainLogical], which substitutes index seeks, applies the
// count-store-gated reorderings and computes the cardinality estimates — now emits
// its lines through a [planLineSink] instead of writing them into a buffer. The
// tree renderer's sink concatenates the line's parts, which is byte for byte what
// it used to write; the table renderer's sink turns the same line into a table
// row. There is exactly one walk, so the two renderings cannot disagree about
// which access path the plan takes.
//
// The profile table reuses [Engine.Profile]'s captured [exec.PlanNode] tree for
// the same reason.

import (
	"context"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/explain"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// planLine is one rendered line of a logical plan, before it is committed to a
// particular presentation.
//
// The split matters because the two presentations need different parts of it:
// the tree renderer concatenates prefix+connector+text+annot, while the table
// renderer puts prefix+connector+text in its Operator column and takes the
// NUMBER out of est for its Est.Rows column rather than the formatted annot.
type planLine struct {
	// prefix is the indentation inherited from the node's ancestors.
	prefix string
	// connector is the node's own tree connector: "" at the root, "└─ " for a
	// last child, "├─ " otherwise.
	connector string
	// text is the operator's display name plus any inline detail — the scanned
	// label, the expand pattern — exactly as the tree prints it.
	text string
	// annot is the rendered cardinality-estimate suffix, empty when the node
	// carries no estimate. It is the tree's presentation of est.
	annot string
	// est is the estimate behind annot; hasEst reports whether one was derived
	// at all. An estimate WAS derived but classified estFallback still has
	// hasEst true and an empty annot: the renderer omits a stale statistic
	// rather than printing a fabricated exact.
	est    estimate
	hasEst bool
	// node is the plan node the line describes. It is the REPLACED child for a
	// line the renderer synthesises in a rewrite's place (the range-seek and
	// min-label leaves), so the line still reports the variables that are bound
	// there, and nil when no node corresponds.
	node ir.LogicalPlan
}

// planLineSink receives each line of a logical-plan walk in render order.
type planLineSink func(planLine)

// estCell renders the line's estimate for the Est.Rows column.
//
// The receiver is a pointer only to avoid copying a 104-byte planLine per call;
// nothing here mutates it.
//
// The cell distinguishes three states, which the tree's annotation also
// distinguishes and which must not be collapsed:
//
//   - "42"  — an exact, maintained count (estExact).
//   - "~42" — a derived estimate: a statistics or heuristic figure. The tilde is
//     the same marker the tree uses ("est. rows~42"), so the two agree.
//   - "-"   — no estimate: either none is derivable for this operator shape, or
//     the statistic behind it is absent or stale (estFallback). Printing a
//     number there would fabricate one.
//
// A genuine estimate of ZERO renders as "0", not "-": an operator the planner
// expects to emit no rows is a real estimate and usually the interesting one.
func (l *planLine) estCell() string {
	if !l.hasEst || l.est.source == estFallback {
		return "-"
	}
	n := strconv.FormatInt(estRows(l.est.rows), 10)
	if l.est.source == estExact {
		return n
	}
	return "~" + n
}

// varsCell renders the variables the line's operator exposes, comma-joined in
// [ir.LogicalPlan.Vars] order. Like [planLine.estCell] the receiver is a pointer
// purely to avoid the copy; nothing here mutates it.
//
// A line with no corresponding plan node renders an
// empty cell rather than a placeholder: it has no variables to report, which is
// not the same as reporting none.
func (l *planLine) varsCell() string {
	if l.node == nil {
		return ""
	}
	return strings.Join(l.node.Vars(), ", ")
}

// explainInputs bundles the live planner state a logical-plan walk reads: the
// index manager and graph view it probes for access-path rewrites, the label
// resolver behind the cardinality estimates, the count-store-gated swap sets, and
// the two Engine gates that decide whether a rewrite may render at all.
//
// It exists so [Engine.ExplainLogical] and [Engine.ExplainTable] compute it the
// same way, from one place, rather than each assembling its own and drifting.
type explainInputs struct {
	plan         ir.LogicalPlan
	idxMgr       *index.Manager
	graph        *lpg.ReadView[string, float64]
	labelSrc     *lpgLabelResolver
	reorderSwaps map[*ir.Apply]bool
	anchorSwaps  map[*ir.Expand]bool
	seekHints    map[*ir.Selection]bool
	prefixSeek   bool
}

// walk drives the shared logical-plan walk into emit.
func (in explainInputs) walk(emit planLineSink, params map[string]expr.Value) {
	explainWithIndexesNode(emit, in.plan, in.idxMgr, params, in.graph, in.labelSrc,
		in.reorderSwaps, in.anchorSwaps, in.seekHints, in.prefixSeek, "", true, true)
}

// ExplainTable returns the LOGICAL plan for query as a Neo4j-style columnar
// table:
//
//	+----------------------------------+----------+------+
//	| Operator                         | Est.Rows | Vars |
//	+----------------------------------+----------+------+
//	| ProduceResults                   |        - | n    |
//	| └─ Projection                    |        - | n    |
//	|    └─ NodeByLabelScan [n:Person] |       40 | n    |
//	+----------------------------------+----------+------+
//
// It is [Engine.ExplainLogical] in a table rather than a tree, and it is the
// SAME walk: the index-seek substitutions, the count-store-gated reorderings and
// the cardinality estimates are computed once and rendered twice, so the two can
// never disagree about the plan. What the table adds is comparability — a column
// of right-aligned estimates reads across operators, where the tree's inline
// annotations do not — and a Vars column the tree does not print at all.
//
// What it loses relative to the tree is the estimate's PROVENANCE tag: the tree
// writes "(est. rows=40, exact)" or "(est. rows~17, stats, err=0.0312)", while
// the column has room only for the number and an approximation marker (see the
// Est.Rows conventions on planLine.estCell). Reach for ExplainLogical when the
// provenance or the certified error term is what you need.
//
// Every figure in the Est.Rows column is an ESTIMATE the planner derived before
// running anything — the column is named for that, and nothing here is measured.
// A cell reading "40" is an exact maintained count of what the operator WOULD
// read, not a count of what it DID; ExplainTable executes nothing.
//
// To see an estimate against what actually happened, use [Engine.ProfileTable],
// which since rmp #2765 carries its OWN Est.Rows column immediately left of the
// measured Rows — the arrangement both incumbents chose (Neo4j orders
// ESTIMATED_ROWS before ROWS in one PROFILE table, renderAsTreeTable.scala:212 at
// 5.26.16; PostgreSQL prints the cost estimate and the "actual" group on one line,
// explain.c:1811 and :1853 at REL_17_STABLE). That closed divergence D3 of
// docs/explain-profile-honesty-audit-2026-09-03.md, which had recorded the manual
// comparison as the audit's largest gap.
//
// The two tables still render two DIFFERENT plans — this one the logical plan,
// ProfileTable the physical one — so they are not read side by side. What they do
// is AGREE, node for node, wherever a logical node has an operator of its own: the
// physical table carries the same estimate this walk derives, computed by the same
// providers through the same helpers. They diverge in exactly one direction, and
// only for the two leaves this renderer SYNTHESISES in a rewrite's place (the
// range-seek scan and the min-label re-anchored scan): those have a line and an
// estimate here, and no logical node for the physical plan to attach one to, so
// they render "-" there. See cypher/plan_estimate_physical.go.
//
// No rows are produced and the graph is not modified. A DDL statement has no
// query plan and renders as a single explanatory row.
func (e *Engine) ExplainTable(query string, params map[string]expr.Value) (s string, err error) {
	defer recoverQueryPanic(&err, "cypher.ExplainTable", "cypher.ExplainTable.panics")
	if ir.IsDDL(query) {
		return explain.FormatPlanTable([]explain.PlanRow{
			{Operator: "(DDL — no query plan)", EstRows: "-"},
		}), nil
	}
	entry, autoParams, perr := e.parseAndAnalyse(query)
	params = mergeAutoParams(params, autoParams)
	if perr != nil {
		return "", perr
	}

	var rows []explain.PlanRow
	e.explainInputsFor(entry, params).walk(func(l planLine) {
		rows = append(rows, explain.PlanRow{
			Operator: l.prefix + l.connector + l.text,
			EstRows:  l.estCell(),
			Vars:     l.varsCell(),
		})
	}, params)
	return explain.FormatPlanTable(rows), nil
}

// ProfileTable executes query and returns the measured PHYSICAL plan as a
// Neo4j-style columnar table:
//
//	+-----------------------------+----------+------+--------+-----------+
//	| Operator                    | Est.Rows | Rows | DbHits | Time (ms) |
//	+-----------------------------+----------+------+--------+-----------+
//	| Project                     |        - |   40 |      ? |     0.032 |
//	| └─ NodeByLabelScan [Person] |       40 |   40 |     40 |     0.003 |
//	+-----------------------------+----------+------+--------+-----------+
//	| Total                       |          |   80 | 40 + ? |     0.032 |
//	+-----------------------------+----------+------+--------+-----------+
//
// The "?" is not a defect in the projection: a Project evaluates a
// caller-supplied expression, and a GoGraph expression can walk the graph (a
// pattern predicate or a pattern comprehension does), so nothing counted what
// this operator may have read. See [exec.PlanNode.DbHitsKnown]. Its "-" in
// Est.Rows is a different statement again: no logical node the planner estimates
// lowers to a final projection, so there is no figure rather than an uncounted one.
//
// # Est.Rows, immediately left of Rows
//
// The first column is the planner's PREDICTION and the second what actually
// happened, adjacent so the pair reads on one line — which is what tells a reader
// whether the plan was chosen on a good guess or a bad one (rmp #2765, closing
// divergence D3, "the largest gap"). The cell conventions are
// [Engine.ExplainTable]'s: a bare number is an exact maintained count, "~N" an
// approximation, "-" no estimate at all. A number is never fabricated, and a
// genuine estimate of ZERO renders "0".
//
// The column appears only when some operator in the plan carries an estimate, and
// the Total row's cell is always blank: estimates are per-operator predictions, and
// a sum over the subset that has one would be a floor presented as a total. Which
// operators have one, and which shapes deliberately cannot, is set out in
// cypher/plan_estimate_physical.go.
//
// It is [Engine.Profile] in a table rather than a tree, over the SAME measured
// plan: ProfileTable and Profile run the identical build-and-drain and render
// one captured [exec.PlanNode] tree two ways. Every caveat documented on Profile
// applies unchanged — the query really runs and its rows are discarded, a
// writing statement is refused rather than executed, and each Time is INCLUSIVE
// of the operator's children, so a node's exclusive cost is its own time minus
// its children's.
//
// The Total line sums every operator's Rows and DbHits across the whole plan and
// reports the ROOT's elapsed time. Read those three cells differently:
//
//   - Total Rows is the rows materialised at every level added together — a cost
//     measure, NOT the result's row count. The result's row count is the ROOT
//     operator's Rows, on the table's first data line.
//   - Total DbHits is the query's total storage-record reads, which is the figure
//     that separates a selective seek from a scan that filtered afterwards. It
//     sums only the cells that are figures, and renders as "x + ?" when any
//     operator's accesses were never counted — so it is a FLOOR in that case, not
//     the whole cost. It is mostly derived rather than counted; see below.
//   - Total Time (ms) is the whole query's elapsed time, because the root's time
//     already includes every child's.
//
// An operator the instrumentation did not reach is marked "(not measured)" in
// its Operator cell rather than left to read as an operator that cost nothing.
//
// # Which of these columns are measurements
//
// Rows and Time (ms) are MEASURED: the profiling wrapper counts the rows an
// operator returned and times its own Next calls.
//
// DbHits is MIXED, and the column says only whether a cell is a figure at all —
// which it does say, since rmp #2760, by printing "?" when it is not. Read a
// number together with the operator's name:
//
//   - For an operator marked [exec.StorageRecordScan] — the scan, seek and
//     single-hop-expand leaves — the cell is DERIVED: it IS the Rows cell, on the
//     contract that such an operator reads one record per row it emits. It can
//     therefore never disagree with Rows, which is why the two columns are equal
//     on every such line.
//   - For [exec.VarLengthExpand] the cell is MEASURED: the operator reports the
//     relationship slots its BFS actually read, which is not its row count. The
//     single-hop expands report the same kind of figure — the adjacency slots they
//     walked, whatever became of each one.
//   - For the morsel-parallel leaves the cell is MEASURED too, since rmp #2762:
//     each reports the node references its workers consumed, so the identical query
//     reports the SAME figure whether it planned a serial scan or a parallel one.
//     Their rows say nothing about it — the aggregate leaf emits one row per group
//     and the count leaf exactly one row, for a walk of the whole node source.
//   - For [exec.AllNodesCountScan] the cell is MEASURED and PATH-AWARE, since
//     rmp #2777: a real 0 when the O(1) live-node counter answers, and one per
//     node id when it declines and the leaf walks the graph to count. The same
//     query therefore reports 0 on a drained substrate and a full walk while
//     another transaction holds an uncommitted create.
//   - For an operator that opens no access path — Limit, Skip, Distinct, Eager,
//     the aggregations, the Apply family — the cell is a KNOWN 0.
//   - For every other operator the cell is "?": nothing counted its accesses.
//     That covers [exec.ShortestPath], [exec.AllShortestPaths],
//     [exec.LabelCountScan] — whose own fallback resolves a filtered bitmap below
//     the resolver interface, where the operator cannot see it — and every
//     operator holding a caller-supplied expression closure that can reach the
//     graph (Filter, Project, Sort, Top, Unwind, the hash joins, RollUpApply,
//     ProcedureCallOp).
//
// In every case the column counts ACCESS-PATH record reads and never property
// reads, which is a documented divergence from Neo4j (see docs/cypher.md). Neo4j
// counts real kernel cursor accesses and charges a hit for a record it read and
// then rejected; GoGraph charges nothing for a rejected read, so a filtered plan
// reads cheaper here than it was. The gaps are enumerated on
// [exec.StorageRecordScan] and audited in
// docs/explain-profile-honesty-audit-2026-09-03.md.
//
// The morsel-parallel tier is reported as ONE node, by construction rather than
// by omission: a parallel leaf builds and drives a private sub-plan per morsel on
// a worker goroutine, the builder clears the profiler from the per-worker build
// options so no worker times anything, and the leaf implements no PlanChildren so
// the tree stops there. All three of its figures are totals for the whole parallel
// phase: ROWS and TIME as measured on the driving goroutine, and — since rmp #2762
// — DB-HITS as counted by the workers themselves, one per node reference consumed,
// folded from a worker-local by a single atomic add per morsel. Its Operator cell
// says the phase is on one node, which is what a reader needs in order not to go
// looking for the filter and projection fused inside it. PostgreSQL takes the other
// route — per-worker sub-entries and a per-worker-average headline — and the
// "parallel tier" section of the [exec.Profiler] documentation records, with
// citations, why that is deliberately not followed here.
func (e *Engine) ProfileTable(ctx context.Context, query string, params map[string]expr.Value) (s string, err error) {
	defer recoverQueryPanic(&err, "cypher.ProfileTable", "cypher.ProfileTable.panics")
	tree, err := e.profilePlanTree(ctx, query, params)
	if err != nil {
		return "", err
	}
	return explain.FormatReport(profileReportFromPlan(&tree)), nil
}

// profileReportFromPlan flattens a measured plan tree into the report the
// columnar formatter consumes, in the same depth-first order and with the same
// tree connectors the indented renderer uses.
func profileReportFromPlan(root *exec.PlanNode) explain.ProfileReport {
	rep := explain.ProfileReport{
		ElapsedMs: float64(root.Time.Nanoseconds()) / 1e6,
	}
	anyMeasured := anyPlanNodeProfiled(root)
	var walk func(n *exec.PlanNode, prefix, childPrefix string)
	walk = func(n *exec.PlanNode, prefix, childPrefix string) {
		name := prefix + n.Name
		if n.Detail != "" {
			name += " [" + n.Detail + "]"
		}
		if anyMeasured && !n.Profiled {
			// Same honesty the indented renderer applies: a bare row in a
			// measured plan would read as an operator that cost nothing.
			name += " (not measured)"
		}
		rep.Operators = append(rep.Operators, explain.OperatorStats{
			Name:        name,
			Rows:        nonNegative(n.Rows),
			DbHits:      nonNegative(n.DbHits),
			DbHitsKnown: n.DbHitsKnown,
			// Carried through with its flag, exactly as DbHits is: the table's
			// Removed column exists only when some operator reports the figure, and
			// a cell is blank rather than 0 for one that does not (rmp #2764). No
			// plan-wide total accompanies it — see explain.FormatReport for why.
			RowsRemovedByFilter:      nonNegative(n.RowsRemovedByFilter),
			RowsRemovedByFilterKnown: n.RowsRemovedByFilterKnown,
			// The planner's PREDICTION, carried through with its provenance exactly as
			// the two measured-or-not figures above are. It renders in its own column
			// immediately left of Rows, so the prediction and the outcome are read on one
			// line (rmp #2765). An operator no logical node claimed has the zero value
			// here, which the formatter renders "-" and never as a number.
			Est:       n.Est,
			ElapsedNs: n.Time.Nanoseconds(),
		})
		rep.TotalRows += nonNegative(n.Rows)
		// The total sums the KNOWN cells only and records that it did. Adding an
		// unknown operator's placeholder zero would silently understate the query's
		// cost as a complete figure; skipping it without saying so would do the
		// same. Neo4j resolves it the same way — TotalHits ORs an `uncertain` flag
		// across the plan and renders "x + ?" (renderSummary.scala, 5.26.16).
		if n.DbHitsKnown {
			rep.TotalDbHits += nonNegative(n.DbHits)
		} else {
			rep.TotalDbHitsUncertain = true
		}
		for i := range n.Children {
			branch, cont := "├─ ", "│  "
			if i == len(n.Children)-1 {
				branch, cont = "└─ ", "   "
			}
			walk(&n.Children[i], childPrefix+branch, childPrefix+cont)
		}
	}
	walk(root, "", "")
	return rep
}

// anyPlanNodeProfiled reports whether any node in the tree carries measurements,
// which is what makes an unmeasured sibling worth labelling.
func anyPlanNodeProfiled(n *exec.PlanNode) bool {
	if n.Profiled {
		return true
	}
	for i := range n.Children {
		if anyPlanNodeProfiled(&n.Children[i]) {
			return true
		}
	}
	return false
}

// nonNegative converts a measured counter to the unsigned form the report
// carries. A negative count cannot arise from the instrumentation — rows and
// db-hits only increment — but the conversion is guarded rather than trusted,
// because an unguarded int64→uint64 cast turns a stray -1 into 1.8e19 and would
// print that in a user-facing table.
func nonNegative(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}
