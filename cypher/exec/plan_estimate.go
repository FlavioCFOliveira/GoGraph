package exec

import "strconv"

// plan_estimate.go — the planner's cardinality estimate carried onto a PHYSICAL
// plan node (rmp #2765).
//
// # Why this exists
//
// GoGraph could render the planner's estimate and the measured row count, but
// never together: `Engine.ExplainTable` renders the LOGICAL plan with an
// Est.Rows column and executes nothing, while `Engine.ProfileTable` renders the
// PHYSICAL plan with the measured Rows. Two tables over two different plans,
// whose lines do not correspond one to one — so a reader could not tell a plan
// chosen on a good guess from one chosen on a bad guess. That is divergence D3 of
// docs/explain-profile-honesty-audit-2026-09-03.md §5, which calls it the largest
// gap.
//
// Both incumbents make the comparison automatic, read in source:
//
//   - Neo4j 5.26.16 (commit 679feffbfb7a9189aba360ea98eef7fc3371e275) puts
//     `Estimated Rows` IMMEDIATELY LEFT of `Rows` in one PROFILE table —
//     renderAsTreeTable.scala:212 lists the column order
//     (OPERATOR, ID, DETAILS, ESTIMATED_ROWS, ROWS, HITS, …), and
//     RenderAsTreeTableTest.scala:277 asserts the header verbatim. A plan node
//     carrying no EstimatedRows argument leaves the cell BLANK rather than
//     printing 0 (same test, the `PARENT` row), and a column no node carries at
//     all is dropped entirely (renderAsTreeTable.scala:47, the
//     `Header.ALL.filter(table.columnLengths.contains)`).
//   - PostgreSQL REL_17_STABLE (commit 018bfcfd9fa4e520970ba3bda370f78bb473c365)
//     prints both on ONE line: the estimate at explain.c:1809-1811
//     (`(cost=%.2f..%.2f rows=%.0f width=%d)`, guarded by `if (es->costs)`) and
//     the measurement at explain.c:1851-1853
//     (`(actual time=%.3f..%.3f rows=%.0f loops=%.0f)`, guarded by `es->analyze`).
//     The word `actual` is what stops the second group being read as the first.
//
// # What is carried, and what is NOT
//
// A [PlanEstimate] is the estimate the planner derived for the LOGICAL node whose
// lowering produced this operator as its ROOT. It is never an estimate computed
// for the physical operator, because no such thing exists: GoGraph's estimate
// providers read label counts, count-store degree cells and property statistics
// against the logical plan, and the physical build consumes those same numbers.
//
// The attribution rule is therefore structural rather than inferred, and the
// cypher package enforces it at the single point where a logical node and its
// built operator are both in hand: the FIRST (deepest) logical node whose
// lowering returns a given operator owns it. A parent that merely passes a
// child's operator through — a Selection whose seek hint was dropped, an Expand
// with no graph — never re-attributes, because the child claimed the operator
// first. See cypher's planEstimates.
//
// Operators with NO estimate are the norm, not the exception, and they render as
// "-" (table) or as nothing at all (tree). Three families cannot be mapped and
// are enumerated on cypher's recordPlanEstimate.

// PlanEstimate is the planner's cardinality estimate for one physical operator,
// together with the provenance that says how far the number may be trusted.
//
// Its zero value is "no estimate", which is what a node nobody attributed one to
// carries and what every renderer prints as an absence.
type PlanEstimate struct {
	// Rows is the estimated row count, already rounded to a whole number. It is
	// meaningful only when Source is not [EstimateAbsent].
	Rows int64
	// Source is the estimate's provenance — how much the number may be trusted.
	Source EstimateSource
}

// EstimateSource is the provenance of a cardinality estimate: where the number
// came from, and therefore how far it may be trusted.
//
// It mirrors the cypher package's own estSource classification
// (docs/optimizer-activation-design.md §2.1) with ONE deliberate collapse: cypher's
// estFallback — an estimate whose backing statistic is absent, dirty or stale —
// arrives here as [EstimateAbsent], because every renderer already treats the two
// identically and must. A stale statistic is not a number a reader may act on, and
// printing it would be exactly the fabrication this whole surface exists to
// prevent. The full four-way classification remains visible in
// [cypher.Engine.ExplainLogical], which prints the provenance tag in words.
type EstimateSource uint8

const (
	// EstimateAbsent is the ZERO VALUE and means there is no estimate: either none
	// was derivable for this operator's shape, or the statistic behind it was
	// absent or stale. No renderer may print a number for it.
	//
	// The zero value is deliberately the absent state. A [PlanNode] assembled by a
	// caller that has not considered estimates at all therefore claims none, which
	// is the honest default — the same reasoning that makes
	// [PlanNode.DbHitsKnown] false by default.
	EstimateAbsent EstimateSource = iota
	// EstimateExact is a maintained, exact count — a label's live-node count, the
	// live node total, an exact count-store degree cell. It is ground truth for the
	// query's pinned snapshot and renders WITHOUT an approximation marker.
	EstimateExact
	// EstimateStats is a histogram- or sample-derived count. It renders with the
	// approximation marker.
	EstimateStats
	// EstimateHeuristic is a principled formula over real inputs (1/NDV × N). It
	// renders with the approximation marker.
	EstimateHeuristic
)

// Known reports whether the source designates an estimate at all.
func (s EstimateSource) Known() bool { return s != EstimateAbsent }

// Exact reports whether the estimate is a maintained exact count rather than an
// approximation. It is what decides the approximation marker in every rendering.
func (s EstimateSource) Exact() bool { return s == EstimateExact }

// String renders the provenance as the same word [cypher.Engine.ExplainLogical]
// prints, so a reader moving between the two surfaces sees one vocabulary. The
// absent state has no word — it is rendered by omitting the figure, never by
// naming it.
func (s EstimateSource) String() string {
	switch s {
	case EstimateExact:
		return "exact"
	case EstimateStats:
		return "stats"
	case EstimateHeuristic:
		return "heuristic"
	case EstimateAbsent:
		return ""
	default:
		return "unknown"
	}
}

// EstRowsUnknown is what a FIXED-WIDTH rendering prints in a cell for which no
// estimate exists.
//
// It is "-" and not "?", and the difference from [DbHitsUnknown] is meant. A "?"
// is an admission that a figure exists and nobody counted it; a "-" says there is
// no figure to have. An operator with no logical counterpart, or one whose
// statistic is stale, is in the second case — and "-" is already the glyph
// [cypher.Engine.ExplainTable]'s Est.Rows column uses for exactly this state, so
// the two tables agree.
//
// Neo4j leaves the cell entirely blank instead (renderAsTreeTable.scala, 5.26.16,
// asserted at RenderAsTreeTableTest.scala:277). GoGraph prints a visible glyph
// because its Est.Rows column is right-aligned next to a right-aligned Rows
// column, where a blank cell reads as a rendering fault rather than as a
// deliberate absence.
const EstRowsUnknown = "-"

// EstRowsCell renders one operator's estimate for a fixed-width table cell:
//
//   - "42"  — an exact, maintained count ([EstimateExact]).
//   - "~42" — an approximation ([EstimateStats] or [EstimateHeuristic]). The tilde
//     is the marker [cypher.Engine.ExplainTable] already uses, and the same one
//     [EstRowsAnnotation] carries into the indented tree.
//   - "-"   — no estimate ([EstimateAbsent] / [EstRowsUnknown]).
//
// A genuine estimate of ZERO renders as "0", never as "-": an operator the planner
// expects to emit no rows is a real estimate, and usually the interesting one.
//
// Every surface goes through this one function — cypher/explain's columnar table
// and the cypher package's own Est.Rows column — so no renderer can print a number
// for an operator that has no estimate by forgetting the source.
func EstRowsCell(e PlanEstimate) string {
	if !e.Source.Known() {
		return EstRowsUnknown
	}
	n := strconv.FormatInt(e.Rows, 10)
	if e.Source.Exact() {
		return n
	}
	return "~" + n
}

// EstRowsAnnotation renders the estimate as the "key=value" pair the INDENTED
// tree prints inside an operator's parenthesis, and reports whether there is one
// to print.
//
// The forms are `est. rows=42 exact`, `est. rows=~17 stats` and
// `est. rows=~17 heuristic`; an absent estimate returns ok=false and the caller
// prints NOTHING — never "-", and never a zero. That is the same rule the
// `removed=` cell follows (rmp #2764): in a free-form line an absent figure is
// omitted, while in a fixed-width table a cell must exist and carries
// [EstRowsUnknown].
//
// Two properties of the wording are load-bearing:
//
//   - The `est. ` prefix is what stops the figure being read as a measurement.
//     Every measured pair on the line is a bare noun (`rows=`, `dbhits=`,
//     `removed=`, `time=`); this is the only one that is qualified, exactly as
//     PostgreSQL qualifies its measured group with the word `actual`
//     (explain.c:1853, REL_17_STABLE) to separate it from the estimate it prints
//     first on the same line.
//   - The value contains no ", " and no "=", so a reader — or a test — splitting
//     the parenthesis on ", " and cutting each field at its first "=" recovers the
//     key `est. rows` and the value `42 exact` unambiguously.
func EstRowsAnnotation(e PlanEstimate) (string, bool) {
	if !e.Source.Known() {
		return "", false
	}
	return "est. rows=" + EstRowsCell(e) + " " + e.Source.String(), true
}
