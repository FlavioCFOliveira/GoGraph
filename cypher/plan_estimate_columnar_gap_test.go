package cypher

// plan_estimate_columnar_gap_test.go — the row-versus-columnar estimate asymmetry
// (rmp #2787).
//
// # What this file pins, and why it is not a duplicate of profile_estimate_test.go
//
// Two shipped documents stated a FALSE CAUSE for the missing estimates on a
// columnar plan:
//
//   - docs/explain-profile-honesty-audit-2026-09-05.md said the absence was
//     "visible on the logical plan too, so it is not a mapping loss";
//   - release-notes/v0.14.0.md listed the columnar fusion chains among four plan
//     shapes that "have no logical node at all".
//
// Both are false, and one query pair disproves them: the SAME logical plan, with
// the SAME `est. rows=1, exact` on the same [ir.Selection], renders that number on
// the row physical plan and loses it on the columnar one. The logical node exists,
// it carries a figure, and the loss is in the MAPPING from logical node to
// operator — not in the estimator's coverage.
//
// profile_estimate_test.go holds what #2765 BUILT. This file holds what it did not
// reach, so that the corrected wording cannot silently drift back.
//
// # This gate asserts a GAP, so it is expected to fail when the gap is fixed
//
// That is deliberate and it is the point. The gap is real at this tree and the two
// documents now describe it accurately; a change that closes it must therefore also
// correct those documents, and this failing gate is what forces that to happen in
// the same commit rather than two releases later.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The two queries differ in ONE thing: what the RETURN projects. Everything the
// planner reasons about — the label, the predicate, the statistics — is identical,
// which is what makes the difference in their physical plans attributable to the
// projection's effect on the LOWERING and to nothing else.
const (
	estGapRowQuery      = `MATCH (c:Component) WHERE c.tag = 'rare' RETURN c`
	estGapColumnarQuery = `MATCH (c:Component) WHERE c.tag = 'rare' RETURN c.key`

	// The fixture's two exact maintained counts, written out so a fixture drift
	// fails loudly instead of weakening the assertion to "some number".
	estGapSelectionRows = 1
	estGapScanRows      = 61
)

// seedEstimateGapGraph builds one `:Component {tag:'rare'}` among 60
// `{tag:'common'}` and refreshes the statistics, so that the equality predicate
// lands in the most-common-value list and the Selection's estimate is EXACT.
//
// An approximate estimate would weaken the gate: "~1" and "-" are both absences of
// certainty, and the asymmetry this file exists to hold is between a figure the
// planner is sure of and no figure at all.
func seedEstimateGapGraph(t *testing.T) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	add := func(key, tag string) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Component"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "tag", lpg.StringValue(tag)); err != nil {
			t.Fatalf("SetNodeProperty(%s, tag): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "key", lpg.StringValue(key)); err != nil {
			t.Fatalf("SetNodeProperty(%s, key): %v", key, err)
		}
	}
	add("c-rare", "rare")
	for i := 0; i < estGapScanRows-1; i++ {
		add(fmt.Sprintf("c-%d", i), "common")
	}
	e := NewEngine(g)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	return e
}

// estGapLine returns the first line of tree whose operator name is exactly op —
// matched on the name that follows the indent glyphs, so "Filter" does not also
// match "ColumnarFilter". found=false is a HARNESS failure in every caller.
func estGapLine(tree, op string) (string, bool) {
	for _, l := range strings.Split(tree, "\n") {
		bare := strings.TrimLeft(l, " │└├─")
		if bare == op || strings.HasPrefix(bare, op+" ") {
			return l, true
		}
	}
	return "", false
}

// TestPlanEstimateColumnarGap_TheLogicalNodeExistsAndCarriesTheEstimateInBoth is
// the first half of the refutation: the claim that the columnar chains "have no
// logical node at all" is false, because the two queries produce the SAME logical
// plan and its Selection carries the SAME exact estimate in both.
//
// Without this half the second half would be consistent with the published text: a
// physical plan can hardly lose a number that was never derived.
func TestPlanEstimateColumnarGap_TheLogicalNodeExistsAndCarriesTheEstimateInBoth(t *testing.T) {
	t.Parallel()
	e := seedEstimateGapGraph(t)
	wantSel := fmt.Sprintf("Selection (est. rows=%d, exact)", estGapSelectionRows)
	wantScan := fmt.Sprintf("NodeByLabelScan [c:Component] (est. rows=%d, exact)", estGapScanRows)

	for _, q := range []string{estGapRowQuery, estGapColumnarQuery} {
		logical, err := e.ExplainLogical(q, nil)
		if err != nil {
			t.Fatalf("ExplainLogical(%q): %v", q, err)
		}
		if !strings.Contains(logical, wantSel) {
			t.Errorf("the logical plan of %q does not carry %q.\n"+
				"Both queries must derive the SAME Selection estimate, or the row-versus-"+
				"columnar comparison in this file compares two different planner beliefs "+
				"rather than two lowerings of one:\n%s", q, wantSel, logical)
		}
		if !strings.Contains(logical, wantScan) {
			t.Errorf("the logical plan of %q does not carry %q, so the fixture's label count "+
				"is not what this file asserts:\n%s", q, wantScan, logical)
		}
	}
}

// TestPlanEstimateColumnarGap_ColumnarLosesTheEstimateTheRowPlanKeeps is the
// second half: over the identical logical plan asserted above, the estimate
// SURVIVES the row lowering and does NOT survive the columnar one.
//
// The shared NodeByLabelScan is asserted to carry the same figure on both physical
// plans. That is the control: it rules out the reading that the columnar arm was
// simply planned against different statistics, which would make the asymmetry an
// artefact of the fixture rather than a property of the lowering.
func TestPlanEstimateColumnarGap_ColumnarLosesTheEstimateTheRowPlanKeeps(t *testing.T) {
	t.Parallel()
	e := seedEstimateGapGraph(t)
	wantScan := fmt.Sprintf("est. rows=%d exact", estGapScanRows)

	rowTree, err := e.Explain(estGapRowQuery, nil)
	if err != nil {
		t.Fatalf("Explain(%q): %v", estGapRowQuery, err)
	}
	colTree, err := e.Explain(estGapColumnarQuery, nil)
	if err != nil {
		t.Fatalf("Explain(%q): %v", estGapColumnarQuery, err)
	}

	// The two arms must actually be the two lowerings this file is about.
	filterLine, okFilter := estGapLine(rowTree, "Filter")
	if !okFilter {
		t.Fatalf("%q did not plan a row Filter, so the row arm of this gate covers "+
			"nothing:\n%s", estGapRowQuery, rowTree)
	}
	colFilterLine, okColFilter := estGapLine(colTree, "ColumnarFilter")
	if !okColFilter {
		t.Fatalf("%q did not plan a ColumnarFilter, so the columnar arm of this gate "+
			"covers nothing:\n%s", estGapColumnarQuery, colTree)
	}

	// The control: same statistics on both plans.
	for name, tree := range map[string]string{"row": rowTree, "columnar": colTree} {
		scanLine, okScan := estGapLine(tree, "NodeByLabelScan")
		if !okScan {
			t.Fatalf("the %s plan has no NodeByLabelScan:\n%s", name, tree)
		}
		if !strings.Contains(scanLine, wantScan) {
			t.Fatalf("the %s plan's scan line %q does not carry %q. The two arms are no "+
				"longer planned against the same statistics, so any difference between them "+
				"is not attributable to the lowering:\n%s", name, scanLine, wantScan, tree)
		}
	}

	// The asymmetry itself.
	wantSel := fmt.Sprintf("est. rows=%d exact", estGapSelectionRows)
	if !strings.Contains(filterLine, wantSel) {
		t.Errorf("the row plan's Filter line %q does not carry %q, so the estimate does not "+
			"survive even the ROW lowering and this file's premise is gone:\n%s",
			filterLine, wantSel, rowTree)
	}
	if strings.Contains(colFilterLine, "est. rows") {
		t.Errorf("the ColumnarFilter line %q now carries an estimate.\n"+
			"THE GAP THIS FILE PINS IS CLOSED — which is good news, and it means two "+
			"documents are now wrong in the other direction. Update, in the same commit:\n"+
			"  * docs/explain-profile-honesty-audit-2026-09-05.md (§1, §5 D3, §8 item 10, "+
			"and the 2026-09-08 addendum)\n"+
			"  * release-notes/v0.14.0.md (the Est.Rows section and its erratum)\n"+
			"then delete or invert this gate.", colFilterLine)
	}
}

// TestPlanEstimateColumnarGap_TheColumnarFilterIsNeverCLAIMED pins the MECHANISM,
// not just the symptom, because the symptom alone is consistent with the published
// (false) explanation that the estimator simply had nothing for the node.
//
// [buildOperator] is the single funnel where a logical node and its operator are
// both in hand, and it records a claim for EVERY operator it returns — including an
// explicitly empty claim when no estimate is derivable. So the sink distinguishes
// the two candidate causes outright:
//
//   - an operator PRESENT in the sink with an empty estimate is estimator coverage:
//     a logical node claimed it and the estimator had no figure for that node type;
//   - an operator ABSENT from the sink was never claimed by any logical node, so no
//     figure could have been looked up for it however good the estimator became.
//
// The row Filter is the first. The ColumnarFilter is the second, and that is what
// makes the absence a mapping loss.
func TestPlanEstimateColumnarGap_TheColumnarFilterIsNeverCLAIMED(t *testing.T) {
	t.Parallel()
	e := seedEstimateGapGraph(t)

	claims := func(q string) map[string]exec.PlanEstimate {
		t.Helper()
		entry, autoParams, err := e.parseAndAnalyse(q)
		if err != nil {
			t.Fatalf("parseAndAnalyse(%q): %v", q, err)
		}
		params := mergeAutoParams(nil, autoParams)
		snap := e.g.BeginRead()
		defer e.g.EndRead(snap)
		sink := planEstimatesFor(entry.plan)
		if _, _, err := e.buildReadPhysical(context.Background(), entry, entry.plan, params,
			newNowAwareRegistry(e.reg, time.Now()), nil, snap, sink); err != nil {
			t.Fatalf("buildReadPhysical(%q): %v", q, err)
		}
		out := make(map[string]exec.PlanEstimate, len(sink.est))
		for op, est := range sink.est {
			name := fmt.Sprintf("%T", op)
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			out[name] = est
		}
		return out
	}

	rowClaims := claims(estGapRowQuery)
	if est, ok := rowClaims["Filter"]; !ok {
		t.Errorf("the row build recorded no claim for Filter, so the sink is not the "+
			"instrument this gate believes it is. Claims: %v", rowClaims)
	} else if est.Source == exec.EstimateAbsent {
		t.Errorf("the row build claimed Filter with an EMPTY estimate; the row arm must " +
			"carry the Selection's figure for the comparison below to mean anything")
	}

	// The columnar arm must actually PLAN a ColumnarFilter. Without this guard the
	// unclaimed assertion below is satisfied by a build that planned no columnar
	// chain at all, which is the one way this gate could pass while covering
	// nothing (measured: neutralisation N1 of rmp #2787).
	colTree, err := e.Explain(estGapColumnarQuery, nil)
	if err != nil {
		t.Fatalf("Explain(%q): %v", estGapColumnarQuery, err)
	}
	if _, ok := estGapLine(colTree, "ColumnarFilter"); !ok {
		t.Fatalf("%q did not plan a ColumnarFilter, so an absent claim for one proves "+
			"nothing:\n%s", estGapColumnarQuery, colTree)
	}

	colClaims := claims(estGapColumnarQuery)
	if _, claimed := colClaims["ColumnarFilter"]; claimed {
		t.Errorf("the columnar build now records a claim for ColumnarFilter. The mapping "+
			"gap this file pins has moved; re-read cypher/plan_estimate_physical.go and the "+
			"two documents it is cited in before changing this gate. Claims: %v", colClaims)
	}
	// The ColumnarProject IS claimed — by the ir.Projection, with an empty estimate,
	// exactly as the row Project is. Asserting it here is what stops the file from
	// being read as "everything columnar is unmapped": the loss is one operator wide.
	if est, claimed := colClaims["ColumnarProject"]; !claimed {
		t.Errorf("the columnar build recorded no claim for ColumnarProject, so the "+
			"one-operator-wide scope this file documents is wrong. Claims: %v", colClaims)
	} else if est.Source != exec.EstimateAbsent {
		t.Errorf("ColumnarProject now carries an estimate (%v); the ir.Projection had none "+
			"when this gate was written", est)
	}
}
