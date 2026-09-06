package exec

// rows_removed_classification_test.go — the drift gate on which operators report
// the rows they removed (rmp #2764).
//
// # Why this census is warranted, when a two-value classification looks too small
// # to need one
//
// The db-hits census next door (dbhits_classification_test.go) classifies four
// states across every operator, and its value is obvious. This one has two —
// REPORTS and SILENT — and the argument for building it anyway is that the
// INTERESTING half is the silent one, and the silence is a JUDGEMENT.
//
// Several operators in this package genuinely discard rows and deliberately do not
// report it. [Limit] and [Skip] drop rows on a count. [Distinct] and the
// aggregations collapse them. [SemiApply] and [AntiSemiApply] drop an outer row on
// whether an inner plan yielded anything. [ExpandIntersect] and
// [IndexNestedLoopJoin] discard candidates per outer row. Every one of those could
// plausibly have been folded into a "rows removed" figure, and each was excluded
// for a reason. Without a census, an exclusion and an OVERSIGHT are the same thing:
// both look like an operator with no marker method. This file is what makes them
// different, and it is why the reason field below is mandatory and separately
// enforced.
//
// The mechanism is the db-hits census's: derive the operator set from the package
// SOURCE with parseOperatorMethodSets, compute each operator's class from the
// methods it actually has (own or promoted through embedding), and compare against
// the hand-written census. A new operator, a rename, or a marker added or removed
// fails the build until somebody writes down the decision.

import (
	"sort"
	"strings"
	"testing"
)

// rowsRemovedClass names one of the two cells an operator can report.
type rowsRemovedClass string

const (
	classReportsRemoved rowsRemovedClass = "REPORTS (rowsRemovedCounter)"
	classSilentRemoved  rowsRemovedClass = "SILENT (no marker; the cell is omitted)"
)

// rowsRemovedCensus is the classification of every operator in this package, with
// the reason each entry is what it is.
//
// The SILENT reasons carry the weight, and they fall into four groups. The wording
// of each is the audit trail for a scope decision taken in rmp #2764:
//
//   - REMOVES NOTHING — the operator emits one row per row it consumed, or produces
//     rows from nothing at all. There is no figure to report and never will be.
//   - DISCARDS, BUT NOT BY A PREDICATE — LIMIT, SKIP, DISTINCT and the aggregations.
//     A row a LIMIT never pulled was removed by nothing; a row DISTINCT collapsed
//     was merged, not rejected; an aggregation's inputs are consumed, not discarded.
//     Folding any of them into a figure named after a filter would make the number
//     describe something a reader cannot act on.
//   - REJECTS BY A PREDICATE AND IS NOT YET COUNTED — a real gap, listed as such so
//     it is visible rather than implied. These are candidates for a follow-up.
//   - DRIVES AN INNER PLAN — the rejection, if any, happens in operators that are
//     measured in their own right and appear as this one's children.
var rowsRemovedCensus = map[string]struct {
	class  rowsRemovedClass
	reason string
}{
	// ── REPORTS ────────────────────────────────────────────────────────────────
	"Filter":         {classReportsRemoved, "counts the rows its predicate rejected, on the reject branch it already takes; NULL and FALSE both count (openCypher 9 §4.1.3)"},
	"ColumnarFilter": {classReportsRemoved, "embeds Filter and shares ONE counter across the boxed Next path and the columnar FillChunk path, which reject in different places"},
	"Expand":         {classReportsRemoved, "counts the adjacency slots its type filter, cyphermorphism check, self-loop deduplication and expand-into comparison discarded; composes exactly with storageAccesses (rows + removed == slots walked)"},
	"columnarExpand": {classReportsRemoved, "embeds *Expand and inherits its counter"},
	"OptionalExpand": {classReportsRemoved, "forwards the inner Expand's count; the inner operator is private to it and is never a node of the rendered plan, so nobody else could report it"},

	// ── SILENT: removes nothing ────────────────────────────────────────────────
	"AllNodesScan":         {classSilentRemoved, "a scan emits every node it reads"},
	"NodeByLabelScan":      {classSilentRemoved, "emits every node of the label"},
	"NodeByIndexSeek":      {classSilentRemoved, "emits every posting the seek returned; the index did the selection, which is exactly the contrast this figure exists to draw"},
	"NodeByIndexSeekSet":   {classSilentRemoved, "as NodeByIndexSeek, over a set of keys"},
	"NodeByIndexRangeScan": {classSilentRemoved, "emits every posting in the range"},
	"AllNodesCountScan":    {classSilentRemoved, "emits one row holding a count"},
	"LabelCountScan":       {classSilentRemoved, "emits one row holding a count"},
	"Argument":             {classSilentRemoved, "re-emits the outer row its Apply driver set"},
	"SingleRow":            {classSilentRemoved, "emits one empty row"},
	"singleRow":            {classSilentRemoved, "emits one caller-supplied row"},
	"StaticRows":           {classSilentRemoved, "emits rows built before execution"},
	"Project":              {classSilentRemoved, "one row out per row in; it reshapes rows, never drops them"},
	"ColumnarProject":      {classSilentRemoved, "as Project, column-major"},
	"Sort":                 {classSilentRemoved, "reorders its input and emits all of it"},
	"Unwind":               {classSilentRemoved, "expands a list into rows; it multiplies rather than removes"},
	"Eager":                {classSilentRemoved, "buffers and re-emits its child's rows"},
	"UnionAll":             {classSilentRemoved, "concatenates two inputs"},
	"ProcedureCallOp":      {classSilentRemoved, "emits whatever the procedure yields; it applies no predicate of its own"},

	// ── SILENT: discards, but not by a predicate ───────────────────────────────
	"Limit":                  {classSilentRemoved, "stops at a build-time count; the rows it never pulled were removed by nothing, and reporting them as rejections would misdescribe the plan"},
	"ColumnarLimit":          {classSilentRemoved, "embeds Limit; same reason"},
	"Skip":                   {classSilentRemoved, "discards a build-time count of leading rows, for a reason no predicate decided"},
	"Top":                    {classSilentRemoved, "a Sort with a Limit; the discarded rows lost a ranking, they were not rejected"},
	"Distinct":               {classSilentRemoved, "collapses duplicates; a merged row was not rejected, and DISTINCT is explicitly out of scope for this figure"},
	"Union":                  {classSilentRemoved, "a Distinct over a UnionAll; same reason"},
	"CountRows":              {classSilentRemoved, "consumes its child's rows into one count; consumption is not rejection"},
	"EagerAggregation":       {classSilentRemoved, "consumes rows into groups; its input rows are absorbed, not discarded"},
	"GlobalAggregateAdapter": {classSilentRemoved, "forwards rows, or emits neutral values for an empty child"},

	// ── SILENT: rejects by a predicate and is NOT yet counted (a real gap) ──────
	"HashJoin":            {classSilentRemoved, "GAP: a probe row that matches no build-side key is discarded by a join condition. PostgreSQL reports that under a DIFFERENT label (`Rows Removed by Join Filter`), which is the shape a follow-up should take rather than folding it in here"},
	"ColumnarHashJoin":    {classSilentRemoved, "GAP: as HashJoin"},
	"ExpandIntersect":     {classSilentRemoved, "GAP: discards candidate neighbours that are not in the intersection; uncounted, as its db-hits are"},
	"IndexNestedLoopJoin": {classSilentRemoved, "GAP: discards outer rows whose per-row seek returns nothing; uncounted, as its db-hits are"},
	"SemiApply":           {classSilentRemoved, "GAP: drops an outer row when its inner plan yields none — an EXISTS filter by any other name. Counting it needs a decision about whether the figure belongs on the driver or on the inner plan, which is out of scope for #2764"},
	"AntiSemiApply":       {classSilentRemoved, "GAP: as SemiApply, inverted"},
	"ShortestPath":        {classSilentRemoved, "GAP: its search rejects arcs by type and by the path predicate; it counts the slots it READ (rmp #2763) but not the ones it rejected"},
	"VarLengthExpand":     {classSilentRemoved, "GAP: its bounded BFS rejects arcs by relationship type and by relationship uniqueness while walking; it counts the slots it READ (its traversal budget) but not the ones it rejected"},
	"AllShortestPaths":    {classSilentRemoved, "GAP: as ShortestPath"},

	// ── SILENT: drives an inner plan measured in its own right ─────────────────
	"Apply":           {classSilentRemoved, "drives an inner plan whose operators are wrapped and measured separately"},
	"CorrelatedApply": {classSilentRemoved, "as Apply"},
	"OptionalApply":   {classSilentRemoved, "as Apply, padding when the inner plan is empty — it removes no outer row at all"},
	"Foreach":         {classSilentRemoved, "drives an inner plan and emits the outer row unchanged"},
	"RollUpApply":     {classSilentRemoved, "collects an inner plan's rows into a list; the inner plan is measured in its own right and this operator rejects nothing"},

	// ── SILENT: morsel-parallel leaves ─────────────────────────────────────────
	//
	// Each fuses a scan, a filter and a projection into a private per-morsel
	// sub-plan on a worker goroutine. Those sub-plans are neither instrumented nor
	// rendered (see Profiler, "the parallel tier is measured as ONE node"), so the
	// inner Filter's counter is unreachable — and reaching it would mean either a
	// shared atomic per rejected row, which is the contention defect rmp #2649
	// measured, or a per-morsel fold like the one #2762 built for db-hits. The
	// second is possible and is left as a follow-up rather than done here.
	"ParallelScanProject":   {classSilentRemoved, "GAP: its per-morsel sub-plan filters, but the sub-plan is not instrumented; a per-morsel fold like #2762's would be needed"},
	"ParallelAggregateScan": {classSilentRemoved, "GAP: as ParallelScanProject"},
	"ParallelCountScan":     {classSilentRemoved, "GAP: as ParallelScanProject"},

	// ── SILENT: writing operators ──────────────────────────────────────────────
	//
	// A writing operator consumes each input row and performs its write. MERGE
	// searches before creating, which is a lookup rather than a rejection of an
	// input row: every row that enters a MERGE leaves it.
	"CreateNode":         {classSilentRemoved, "creates one node per input row"},
	"CreateRelationship": {classSilentRemoved, "creates one relationship per input row"},
	"DeleteNode":         {classSilentRemoved, "deletes and forwards; it drops no row"},
	"DeleteRelationship": {classSilentRemoved, "deletes and forwards; it drops no row"},
	"DetachDelete":       {classSilentRemoved, "deletes and forwards; it drops no row"},
	"SetProperty":        {classSilentRemoved, "writes and forwards"},
	"SetAllProperties":   {classSilentRemoved, "writes and forwards"},
	"SetLabels":          {classSilentRemoved, "writes and forwards"},
	"RemoveLabels":       {classSilentRemoved, "writes and forwards"},
	"RemoveProperty":     {classSilentRemoved, "writes and forwards"},
	"Merge":              {classSilentRemoved, "searches for a match before creating; every input row leaves the operator"},
	"MergePattern":       {classSilentRemoved, "as Merge, over a pattern"},
	"MergeRelationship":  {classSilentRemoved, "as Merge, over a relationship"},
}

// TestRowsRemovedClassification_EveryOperatorIsClassified fails when the operator
// set and the census above disagree — a new operator, a renamed one, or the marker
// added or removed.
func TestRowsRemovedClassification_EveryOperatorIsClassified(t *testing.T) {
	t.Parallel()

	operators, methods := parseOperatorMethodSets(t)

	classOf := func(name string) rowsRemovedClass {
		if methods[name]["rowsRemovedByFilter"] {
			return classReportsRemoved
		}
		return classSilentRemoved
	}

	var problems []string
	for _, name := range operators {
		want, listed := rowsRemovedCensus[name]
		got := classOf(name)
		switch {
		case !listed:
			problems = append(problems, name+": NOT IN THE CENSUS (source says "+string(got)+")")
		case want.class != got:
			problems = append(problems, name+": census says "+string(want.class)+
				", source says "+string(got))
		}
	}
	present := map[string]bool{}
	for _, n := range operators {
		present[n] = true
	}
	for name := range rowsRemovedCensus {
		if !present[name] {
			problems = append(problems, name+": in the census but is no longer an operator in this package")
		}
	}
	sort.Strings(problems)

	if len(problems) > 0 {
		t.Fatalf("the rows-removed classification and the source disagree in %d place(s):\n  %s\n\n"+
			"Whether an operator reports the rows it removed is decided by whether it "+
			"implements rowsRemovedCounter, and the DEFAULT is silence. Silence is the "+
			"right answer for most operators and the wrong one for a few, and the two "+
			"are indistinguishable in the code — which is why the decision, and its "+
			"REASON, must be written down in rowsRemovedCensus "+
			"(cypher/exec/rows_removed_classification_test.go). Decide, add the method "+
			"if the operator earns it, and record why either way.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestRowsRemovedClassification_CensusReasonsArePresent keeps the census from
// degrading into a bare list. The reason is the audit; without it an entry records
// only that somebody typed a name — and for the SILENT entries, which is most of
// them, the reason is the only thing separating a decision from an oversight.
func TestRowsRemovedClassification_CensusReasonsArePresent(t *testing.T) {
	t.Parallel()
	for name, e := range rowsRemovedCensus {
		if strings.TrimSpace(e.reason) == "" {
			t.Errorf("%s is classified %s with no reason recorded", name, e.class)
		}
	}
}

// TestRowsRemovedClassification_ReportingSetIsNotEmpty is the non-vacuity guard on
// the gate above.
//
// parseOperatorMethodSets derives its operator set from the package source by
// matching a Next or FillChunk signature. If that heuristic ever stopped matching
// the marker method — a rename, a signature change — every operator would classify
// SILENT, the census would have to be rewritten to agree, and the gate would pass
// while reporting nothing. This asserts the reporting set is exactly what the
// compile-time assertions in rows_removed.go declare.
func TestRowsRemovedClassification_ReportingSetIsNotEmpty(t *testing.T) {
	t.Parallel()
	_, methods := parseOperatorMethodSets(t)

	want := map[string]bool{
		"Filter": true, "ColumnarFilter": true,
		"Expand": true, "columnarExpand": true, "OptionalExpand": true,
	}
	got := map[string]bool{}
	for name, ms := range methods {
		if ms["rowsRemovedByFilter"] {
			got[name] = true
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s does not carry rowsRemovedByFilter in the parsed method set. "+
				"Either the operator lost the method or parseOperatorMethodSets can no "+
				"longer see it, and in the second case the classification gate passes "+
				"vacuously", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s carries rowsRemovedByFilter but is not in this test's expected "+
				"set. A new reporting operator is a deliberate widening of the figure's "+
				"scope: add it here and to rowsRemovedCensus with its reason", name)
		}
	}
}
