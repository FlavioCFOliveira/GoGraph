package cypher

// profile_dbhits_unknown_test.go — the UNKNOWN db-hits state (rmp #2760).
//
// PROFILE's DbHits column could not distinguish "counted, and it is zero" from
// "not counted at all": a pure row transformer and a morsel-parallel scan of
// 2000 nodes both printed 0. rmp #2760 gave the figure an explicit unknown state
// and carried it to every renderer. This file is the gate on that state, on both
// sides of it:
//
//   - an operator that provably opens no access path must still print a real 0,
//     or the change would have replaced one uninformative cell with another;
//   - an operator whose accesses nobody counted must print "?";
//   - the plan-wide Total must say when it summed over a gap.
//
// It also records, with a control arm, the finding that forced Filter and
// Project into the UNKNOWN class:
// docs/explain-profile-honesty-audit-2026-09-03.md called Project "a pure row
// transformer [that] reports dbhits=0, honestly", and that is not true — a
// GoGraph expression can walk the graph.
//
// Peer behaviour, read in source at the pinned tags rather than recalled:
// Neo4j 5.26.16 carries OperatorProfile.NO_DATA = -1
// (community/cypher/runtime-util/.../OperatorProfile.java), drops the argument
// when a value equals it (PlanDescriptionBuilder.scala,
// BuildPlanDescription.addArgument), and renders an incomplete total as "x + ?"
// (renderSummary.scala, over InternalPlanDescription.TotalHits, whose `+` ORs the
// `uncertain` flag). PostgreSQL REL_17_STABLE suppresses each zero-valued
// counter individually under the comment "Show only positive counter values."
// (src/backend/commands/explain.c:3764).
//
// Layer: short.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/explain"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// fanGraph builds one :Root with `fan` outgoing :LIKES edges to :Leaf nodes.
// Every gate below turns on that fan being large enough that reading it is
// unmistakable next to a plan that claims a single-digit db-hit total.
func fanGraph(t *testing.T, fan int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	mustRun(t, eng, "CREATE (:Root {k:0})")
	for i := 0; i < fan; i++ {
		mustRun(t, eng, fmt.Sprintf("MATCH (r:Root) CREATE (r)-[:LIKES]->(:Leaf {i:%d})", i))
	}
	return eng
}

// dbHitsCellOf returns the db-hits cell an operator rendered, verbatim, located
// by the prefix of its line in an Engine.Profile tree. It returns the STRING and
// not a parsed number on purpose: the cell is what a reader sees, and a gate on
// the state has to assert the glyph.
func dbHitsCellOf(t *testing.T, plan, operator string) string {
	t.Helper()
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, "│└├─ ")
		if !strings.HasPrefix(trimmed, operator) {
			continue
		}
		i := strings.Index(trimmed, "dbhits=")
		if i < 0 {
			t.Fatalf("the %s line carries no dbhits cell: %q", operator, trimmed)
		}
		rest := trimmed[i+len("dbhits="):]
		end := strings.IndexAny(rest, ",)")
		if end < 0 {
			t.Fatalf("malformed dbhits cell in %q", trimmed)
		}
		return rest[:end]
	}
	t.Fatalf("no %s in the plan, so this gate covers nothing:\n%s", operator, plan)
	return ""
}

// TestProfileDbHits_PureTransformerReportsAKnownZero is the other half of the
// unknown state, and the reason the change is an improvement rather than a
// retreat: an operator that provably opens no access path still reports a real
// zero.
//
// Distinct qualifies exactly: it hashes and compares values already bound in the
// row, holds no caller-supplied expression, and touches no graph handle
// (cypher/exec/distinct.go). It claims exec's noStorageAccess marker for that
// reason, and the census in cypher/exec/dbhits_classification_test.go records
// the claim.
//
// Without this gate the honest fix would have a trivial wrong implementation —
// render "?" for everything that is not a scan — which would be no more readable
// than the zero it replaced.
func TestProfileDbHits_PureTransformerReportsAKnownZero(t *testing.T) {
	t.Parallel()
	eng := fanGraph(t, 20)

	plan, err := eng.Profile(context.Background(),
		"MATCH (r:Root)-[:LIKES]->(l) RETURN DISTINCT r.k", nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}

	if got := dbHitsCellOf(t, plan, "Distinct"); got != "0" {
		t.Errorf("Distinct rendered dbhits=%s, want 0. It opens no access path — it "+
			"hashes values already in the row — so its zero is a MEASUREMENT and must "+
			"not be collapsed into the unknown state that operators reading uncounted "+
			"storage report:\n%s", got, plan)
	}
	// Non-vacuity: the same plan must contain a cell in each of the other two
	// states, or "0" here would prove only that the renderer prints zeros.
	if got := dbHitsCellOf(t, plan, "Expand"); got == "0" || got == exec.DbHitsUnknown {
		t.Errorf("the Expand in the control plan rendered dbhits=%s; it reads one "+
			"relationship slot per emitted row, so a derived figure is expected and "+
			"this gate needs it to prove the zero above is discriminating:\n%s", got, plan)
	}
	if got := dbHitsCellOf(t, plan, "ColumnarProject"); got != exec.DbHitsUnknown {
		t.Errorf("the projection rendered dbhits=%s, want %q; a projection evaluates a "+
			"caller-supplied expression that can walk the graph, so its accesses are "+
			"uncounted:\n%s", got, exec.DbHitsUnknown, plan)
	}
}

// TestProfileDbHits_ExpressionEvaluationIsUncountedStorage is the measurement
// behind the classification decision.
//
// A WHERE pattern predicate is evaluated INSIDE the Filter, by
// cypher's patternEvaluator, which walks the source node's adjacency
// (cypher/pattern_eval.go, EvalPattern → matchLabelledHop / matchPattern). No
// operator exists in the plan for that walk and no counter observes it, so the
// whole plan reports the label scan's single db-hit for a query that cannot be
// answered without reading relationship records.
//
// The CONTROL is the same question asked as a real expansion over the same
// graph: its Expand reports the fan. Subject and control return the same answer
// and must read the same edges; only the reported figure differs. That is what
// makes the Filter's silence a reporting gap and not a fact about the workload —
// and it is why Filter cannot claim exec's noStorageAccess marker.
func TestProfileDbHits_ExpressionEvaluationIsUncountedStorage(t *testing.T) {
	t.Parallel()
	const fan = 100
	eng := fanGraph(t, fan)
	ctx := context.Background()

	subject, err := eng.Profile(ctx, "MATCH (r:Root) WHERE (r)-[:LIKES]->() RETURN r.k", nil)
	if err != nil {
		t.Fatalf("Profile(subject): %v", err)
	}
	control, err := eng.Profile(ctx, "MATCH (r:Root)-[:LIKES]->(l) RETURN DISTINCT r.k", nil)
	if err != nil {
		t.Fatalf("Profile(control): %v", err)
	}

	controlHits, controlUnknown := totalDbHits(t, control)
	if controlHits < fan {
		t.Fatalf("the control arm reports %d db-hits over a %d-way fan; it expands the "+
			"relationships explicitly, so it must report at least the fan — without "+
			"that this comparison has no baseline (unknown cells: %d):\n%s",
			controlHits, fan, controlUnknown, control)
	}

	subjectHits, subjectUnknown := totalDbHits(t, subject)
	if subjectHits >= fan {
		t.Fatalf("the subject arm reports %d db-hits, which is at least the %d-way fan. "+
			"Something now counts the pattern predicate's adjacency walk; if that is "+
			"deliberate, Filter may be able to claim a real figure and the census in "+
			"cypher/exec/dbhits_classification_test.go must be updated with it:\n%s",
			subjectHits, fan, subject)
	}
	if subjectUnknown == 0 {
		t.Errorf("the subject plan reports a COMPLETE db-hits total of %d for a query "+
			"that walked the :Root's adjacency to answer its predicate. The control "+
			"reports %d for the same edges. Every cell being a figure would be the "+
			"engine asserting it read %d records when it read more — the exact claim "+
			"rmp #2760 removed.\nsubject:\n%s\ncontrol:\n%s",
			subjectHits, controlHits, subjectHits, subject, control)
	}
	if got := dbHitsCellOf(t, subject, "Filter"); got != exec.DbHitsUnknown {
		t.Errorf("the Filter rendered dbhits=%s, want %q — its predicate is a pattern "+
			"predicate and nothing counted the adjacency it walked:\n%s",
			got, exec.DbHitsUnknown, subject)
	}
}

// TestProfileDbHits_TotalDeclaresWhatItCouldNotSum is acceptance criterion 3.
//
// A Total that silently summed over uncounted operators would undo the per-cell
// honesty one line lower down the same table: a reader who saw "?" in a cell and
// a plain number in the Total would reasonably conclude the Total had accounted
// for it. Both directions are asserted — incomplete when a cell is unknown, and
// a plain number when none is — because only the pair rules out a renderer that
// simply always appends the marker.
func TestProfileDbHits_TotalDeclaresWhatItCouldNotSum(t *testing.T) {
	t.Parallel()

	t.Run("incomplete when a cell is unknown", func(t *testing.T) {
		t.Parallel()
		eng := fanGraph(t, 20)
		out, err := eng.ProfileTable(context.Background(), "MATCH (l:Leaf) RETURN l.i", nil)
		if err != nil {
			t.Fatalf("ProfileTable: %v", err)
		}
		if !strings.Contains(out, exec.DbHitsUnknown) {
			t.Fatalf("no operator reported an unknown db-hits cell, so this case cannot "+
				"exercise an incomplete total:\n%s", out)
		}
		if !strings.Contains(out, "+ "+exec.DbHitsUnknown) {
			t.Errorf("the Total line does not declare the sum incomplete. Neo4j renders "+
				"this state \"x + ?\" (renderSummary.scala, 5.26.16); a plain number here "+
				"would present a floor as the query's whole storage cost:\n%s", out)
		}
	})

	t.Run("a plain number when every cell is a figure", func(t *testing.T) {
		t.Parallel()
		// Built rather than executed: every physical plan the engine produces today
		// carries a projection at its root, and a projection is UNKNOWN, so no real
		// query can exercise the complete-total arm. Driving the flattener directly
		// is what keeps this direction of the assertion testable at all — and it is
		// the same function ProfileTable calls, not a reimplementation of it.
		tree := exec.PlanNode{
			Name: "Distinct", Profiled: true, Rows: 3, DbHits: 0, DbHitsKnown: true,
			Children: []exec.PlanNode{{
				Name: "NodeByLabelScan", Detail: "Leaf", Profiled: true,
				Rows: 20, DbHits: 20, DbHitsKnown: true,
			}},
		}
		rep := profileReportFromPlan(&tree)
		if rep.TotalDbHitsUncertain {
			t.Errorf("a tree whose every cell is a figure produced an uncertain total")
		}
		if rep.TotalDbHits != 20 {
			t.Errorf("TotalDbHits = %d, want 20", rep.TotalDbHits)
		}
		out := explain.FormatReport(rep)
		if strings.Contains(out, exec.DbHitsUnknown) {
			t.Errorf("the table carries an unknown marker although every cell is a "+
				"figure:\n%s", out)
		}

		// And the same tree with ONE cell taken away must flip both.
		tree.DbHitsKnown = false
		rep = profileReportFromPlan(&tree)
		if !rep.TotalDbHitsUncertain {
			t.Errorf("removing one operator's figure left the total certain")
		}
		if rep.TotalDbHits != 20 {
			t.Errorf("TotalDbHits = %d after removing an unknown operator's contribution, "+
				"want 20: the known cells must still be summed, so the total is a floor "+
				"and not discarded", rep.TotalDbHits)
		}
		if out := explain.FormatReport(rep); !strings.Contains(out, "20 + "+exec.DbHitsUnknown) {
			t.Errorf("the incomplete total does not render \"20 + %s\":\n%s",
				exec.DbHitsUnknown, out)
		}
	})
}

// TestProfileDbHits_UnprofiledNodeIsNotAZero pins the interaction between the
// two honesty states the renderer now carries. A node the instrumentation never
// reached is already labelled "(not measured)"; its db-hits must be unknown too,
// because nothing counted them either. TestProfile_EveryOperatorIsMeasured holds
// that no such node exists in practice, so this is a contract test on the
// flattener rather than an observation of a real plan.
func TestProfileDbHits_UnprofiledNodeIsNotAZero(t *testing.T) {
	t.Parallel()
	tree := exec.PlanNode{
		Name: "Project", Profiled: true, Rows: 1, DbHits: 0, DbHitsKnown: true,
		Children: []exec.PlanNode{{Name: "Mystery"}}, // never instrumented
	}
	rep := profileReportFromPlan(&tree)
	if !rep.TotalDbHitsUncertain {
		t.Errorf("a plan containing an uninstrumented operator produced a CERTAIN total; " +
			"nothing counted that operator's accesses, so the sum is a floor")
	}
	if len(rep.Operators) != 2 {
		t.Fatalf("flattened %d operators, want 2", len(rep.Operators))
	}
	if rep.Operators[1].DbHitsKnown {
		t.Errorf("the uninstrumented operator claims a known db-hits figure")
	}
	if !strings.Contains(rep.Operators[1].Name, "(not measured)") {
		t.Errorf("the uninstrumented operator is not labelled: %q", rep.Operators[1].Name)
	}
}
