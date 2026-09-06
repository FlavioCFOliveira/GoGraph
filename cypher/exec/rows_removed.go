package exec

import "strconv"

// rows_removed.go — the rows-removed-by-filter figure (rmp #2764).
//
// # The question this answers, which no other column can
//
// A [Filter] that consumed 1 000 000 rows and emitted 3 renders identically to
// one that consumed 3 and emitted 3: only the emitted count is printed. So a
// reader cannot tell a SELECTIVE ACCESS PATH from a scan that filtered
// afterwards — which is the exact question db-hits exists to answer and cannot
// answer alone, because db-hits is charged on the operator that READ the record
// and says nothing about which operator later threw it away.
// docs/explain-profile-honesty-audit-2026-09-03.md §5 records the gap as
// divergence D2 and calls it "the sharpest missing figure".
//
// # PostgreSQL, read in source at REL_17_STABLE
//
// PostgreSQL prints `Rows Removed by Filter`, and the mechanism was read rather
// than recalled, at commit 018bfcfd9fa4e520970ba3bda370f78bb473c365:
//
//   - The counter is a single `double nfiltered1` on the node's Instrumentation
//     (`src/include/executor/instrument.h:89`, commented "# of tuples removed by
//     scanqual or joinqual"), bumped through the `InstrCountFiltered1` macro
//     (`src/include/nodes/execnodes.h:1223-1227`).
//   - It is bumped ONE PER REJECTED TUPLE, in the `else` arm of the qual test the
//     executor already branches on: `if (qual == NULL || ExecQual(qual, econtext))
//     { ... return slot; } else InstrCountFiltered1(node, 1);`
//     (`src/backend/executor/execScan.c:232-255`). There are 9 such increment
//     sites across the executor and 19 `"Rows Removed by Filter"` print sites in
//     `src/backend/commands/explain.c`.
//   - PRINTING IS GUARDED TWICE. First on the PLAN: every print site is wrapped in
//     `if (plan->qual)` (e.g. `explain.c:2176-2178`), so a node with no filter
//     expression never prints the figure at all — it has none. Second on the RUN:
//     `show_instrumentation_count` returns immediately unless
//     `es->analyze && planstate->instrument` (`explain.c:3628-3629`), so a plain
//     EXPLAIN prints nothing.
//   - A ZERO IS SUPPRESSED IN TEXT MODE, under the comment
//     "In text mode, suppress zero counts; they're not interesting enough"
//     (`explain.c:3638-3639`); structured formats still emit it.
//   - The printed value is `nfiltered / nloops` (`explain.c:3641`) — a PER-LOOP
//     AVERAGE, not a total, and a `double` rather than an integer.
//
// # Where GoGraph follows it, and where it deliberately does not
//
// FOLLOWED: the concept and its name (`RowsRemovedByFilter` on the wire), the
// increment placed in the branch the operator already takes, and the rule that an
// operator which cannot remove rows prints NOTHING rather than a zero — which is
// the house rule rmp #2760 already established for db-hits and which PostgreSQL
// reaches by its `if (plan->qual)` guard.
//
// DIVERGED, on three points, each deliberate:
//
//  1. A MEASURED ZERO IS PRINTED. PostgreSQL suppresses a zero in text mode;
//     GoGraph prints `removed=0` for an operator that CAN remove rows and removed
//     none. The two states are different facts — "this operator has no filter" and
//     "this filter rejected nothing, so it bought you nothing" — and the second is
//     precisely the signal a reader of a slow plan wants. Suppressing it would
//     collapse the distinction this figure exists to draw, and would contradict the
//     tri-state discipline of [PlanNode.DbHitsKnown], where the flag says whether a
//     figure EXISTS and the number then means what it says.
//  2. THE FIGURE IS A LIFETIME TOTAL, never divided. GoGraph prints no `loops`
//     column and its inner-side operators already report lifetime totals across
//     re-Init (audit §5, D5); dividing this one figure by an invocation count the
//     rest of the output does not show would make it incomparable with the Rows
//     beside it.
//  3. THERE IS NO PLAN-WIDE TOTAL. Db-hits has one because every operator is
//     classified, so the sum is either complete or explicitly `x + ?`. This figure
//     is reported by three operator families only, and other operators reject rows
//     for reasons this figure would misdescribe ([SemiApply] and [AntiSemiApply]
//     drop an outer row on an inner plan's emptiness; [ExpandIntersect] and
//     [IndexNestedLoopJoin] discard candidates per outer row). A summed cell would
//     therefore be a floor presented as a total, so no total is rendered at all and
//     the census in rows_removed_classification_test.go records every exclusion
//     with its reason.
//
// # What is deliberately NOT counted, and why
//
// LIMIT, SKIP, DISTINCT and aggregation all discard rows, and none of them
// contributes here. A row a LIMIT never pulled was not removed by anything, a row
// DISTINCT collapsed was not rejected but merged, and an aggregation's input rows
// are consumed rather than discarded. Folding any of them into a figure named
// "removed by filter" would make the number describe something a reader cannot act
// on. They are marked [noStorageAccess] for db-hits and silent here, and the census
// gate records that as a decision rather than an omission.
//
// # The cost, and why this one may be per-record when db-hits may not
//
// [storageAccessCounter] refuses a per-record increment, because a db-hit is
// charged on the path the operator SUCCEEDS on — the one every ordinary query
// runs. This counter is the mirror image: it is charged only on the REJECT branch,
// which the operator has already decided to take and on which it does no other
// work. There is no increment, and no branch, on the accepted path. That is
// PostgreSQL's own placement (`execScan.c:255`), and it is why the two interfaces
// can hold opposite rules without contradiction.

// rowsRemovedCounter is implemented by an operator that reads candidate rows and
// discards some of them because a predicate said no.
//
// A figure reported here is MEASURED and EXACT: it is a count of decisions the
// operator took, never a difference inferred from two other columns. For the
// operators that also count storage accesses the two figures compose exactly —
// see [Expand.rowsRemovedByFilter], where every slot the cursor consumed is either
// removed here or emitted, so `storageAccesses() == rowsRemovedByFilter() +
// admitted`. That identity is what makes the figure falsifiable rather than merely
// plausible, and it was verified with a temporary probe that panicked on
// disagreement.
//
// The method is unexported so only operators in this package can claim to have
// measured their rejections, which keeps the set of reporting operators auditable —
// the same discipline [StorageRecordScan] and [noStorageAccess] follow.
//
// An operator that does not implement it reports NO figure, and every renderer
// omits the cell rather than printing 0. The two states are distinct and the
// distinction is the point: "this operator cannot remove rows" and "this operator
// removed none" are different facts about a plan.
type rowsRemovedCounter interface {
	// rowsRemovedByFilter returns the candidate rows this operator read and
	// discarded over its whole lifetime, including any Init it has been restarted
	// by.
	rowsRemovedByFilter() int64
}

// The census of REPORTING operators. Like the db-hits census blocks in profile.go,
// this exists so the claim is checked at compile time and readable in one place:
// an unexported marker method that does not actually satisfy the interface would
// leave the operator silent while its own godoc said it reported.
// TestRowsRemovedClassification_EveryOperatorIsClassified is the drift gate that
// keeps this list and the hand-written census in
// cypher/exec/rows_removed_classification_test.go from parting company.
var (
	_ rowsRemovedCounter = (*Filter)(nil)
	_ rowsRemovedCounter = (*ColumnarFilter)(nil) // promoted from the embedded Filter
	_ rowsRemovedCounter = (*Expand)(nil)
	_ rowsRemovedCounter = (*columnarExpand)(nil) // promoted from the embedded *Expand
	_ rowsRemovedCounter = (*OptionalExpand)(nil)
)

// RowsRemovedCell renders one operator's rows-removed cell for a fixed-width
// table: the number when the operator reports the figure, and the EMPTY string
// when it does not.
//
// It is blank rather than "?" on purpose, and the two glyphs say different things.
// [DbHitsUnknown]'s "?" means "this operator reads storage and nobody counted it"
// — an admission of a measurement gap. A blank here means "this operator removes
// no rows, so there is no figure to report" — a property of the operator, not a
// gap. Neo4j draws the same distinction by blanking a cell whose plan node carries
// no such argument (renderAsTreeTable.scala, 5.26.16), and PostgreSQL by never
// reaching the print site when the node has no qual (explain.c, `if (plan->qual)`).
//
// Every table surface goes through this one function, so no renderer can print a
// zero for an operator that reports nothing by forgetting the flag.
func RowsRemovedCell(removed int64, known bool) string {
	if !known {
		return ""
	}
	return strconv.FormatInt(removed, 10)
}
