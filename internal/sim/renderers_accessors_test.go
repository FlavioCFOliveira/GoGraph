package sim

import (
	"strings"
	"testing"
)

// TestActorNamesShort pins every actor's Name identifier. The Name methods are
// otherwise only reached on report/label paths the short layer rarely hits, yet
// they are pure and instant to assert, so they belong in the PR smoke.
func TestActorNamesShort(t *testing.T) {
	t.Parallel()
	names := map[string]string{
		"HonestWriter":       HonestWriter{}.Name(),
		"BoundedChurnWriter": BoundedChurnWriter{}.Name(),
		"HonestReader":       HonestReader{}.Name(),
		"MalformedSender":    MalformedSender{}.Name(),
		"BoltAbuser":         BoltAbuser{}.Name(),
		"OverloadActor":      OverloadActor{}.Name(),
		"SchemaChanger":      SchemaChanger{}.Name(),
	}
	for want, got := range names {
		if got != want {
			t.Errorf("actor Name = %q, want %q", got, want)
		}
	}
	if got := NewSlowConsumer(nil).Name(); got != "SlowConsumer" {
		t.Errorf("SlowConsumer Name = %q, want SlowConsumer", got)
	}
}

// TestAbuseFamilyStringsShort renders every BoltAbuser / OverloadActor /
// SchemaChanger family so the String renderers (used in failure reports) stay
// covered. It asserts each family produces a non-empty, non-fallback label and
// that distinct families render distinctly.
func TestAbuseFamilyStringsShort(t *testing.T) {
	t.Parallel()

	check := func(label string, n int, render func(int) string) {
		seen := make(map[string]bool, n)
		for i := 0; i < n; i++ {
			s := render(i)
			if s == "" || strings.Contains(s, "(") {
				t.Errorf("%s family %d rendered as a fallback/empty label: %q", label, i, s)
			}
			if seen[s] {
				t.Errorf("%s family %d duplicates an earlier label %q", label, i, s)
			}
			seen[s] = true
		}
	}
	check("AbuseFamily", abuseFamilyCount, func(i int) string { return AbuseFamily(i).String() })
	check("OverloadFamily", overloadFamilyCount, func(i int) string { return OverloadFamily(i).String() })
	check("SchemaChangeFamily", schemaChangeFamilyCount, func(i int) string { return SchemaChangeFamily(i).String() })
}

// TestExecModeStringShort renders every ExecMode plus the out-of-range fallback.
func TestExecModeStringShort(t *testing.T) {
	t.Parallel()
	cases := map[ExecMode]string{
		ModeDeterministic: "deterministic",
		ModeConcurrent:    "concurrent",
		ModeLiveness:      "liveness",
		ModeBulkVsOnline:  "bulk-vs-online",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("ExecMode(%d).String() = %q, want %q", int(m), got, want)
		}
	}
	if got := ExecMode(99).String(); !strings.Contains(got, "99") {
		t.Errorf("out-of-range ExecMode should render its int: %q", got)
	}
}

// TestRangeSeekVariantPairShort pins the second differential variant pair: the
// default vs the range-index-seek-disabled engine.
func TestRangeSeekVariantPairShort(t *testing.T) {
	t.Parallel()
	a, b := RangeSeekVariantPair()
	if a.Name != "default" {
		t.Errorf("variant A name = %q, want default", a.Name)
	}
	if !b.Options.DisableRangeIndexSeek {
		t.Error("variant B should disable the range index seek")
	}
}

// TestRendererStringsShort exercises the report/summary String renderers that
// only fire on failure paths in normal runs: a SimReport, a Violation, the
// metrics-oracle result, the coverage summary, the differential result, and the
// oracle's compact summary. Rendering them on a synthetic value keeps the
// human-facing failure output covered without provoking a real failure.
func TestRendererStringsShort(t *testing.T) {
	t.Parallel()

	v := Violation{Kind: ViolationACIDConsistency, Tick: 7, Op: "MATCH", Message: "boom"}
	if !strings.Contains(v.String(), "boom") || !strings.Contains(v.String(), "tick=7") {
		t.Errorf("Violation.String missing detail: %s", v.String())
	}

	rep := &SimReport{
		Seed:        42,
		FailedTick:  3,
		FailedOp:    Op{Kind: OpCreate, Cypher: "CREATE (n)"},
		Violations:  []Violation{v},
		OracleState: OracleSnapshot{NodeCount: 1, EdgeCount: 0, OpCount: 2},
	}
	if s := rep.String(); !strings.Contains(s, "Seed:") || !strings.Contains(s, "boom") {
		t.Errorf("SimReport.String missing detail: %s", s)
	}

	mres := &MetricsOracleResult{
		Before:         MetricsSnapshot{RunInTxCount: 1, Goroutines: 10},
		After:          MetricsSnapshot{RunInTxCount: 3, Goroutines: 12},
		ExpectedWrites: 2,
		Discrepancies:  []string{"goroutine leak"},
	}
	if mres.Consistent() {
		t.Error("a result with a discrepancy must not be Consistent")
	}
	if s := mres.String(); !strings.Contains(s, "DISCREPANCY") {
		t.Errorf("MetricsOracleResult.String missing discrepancy: %s", s)
	}

	d := DiffResult{Agreed: false, DivergedAt: 1, VariantA: "a", VariantB: "b", SignatureA: "[x]", SignatureB: "[y]", Reason: "mismatch"}
	if s := d.String(); !strings.Contains(s, "DIVERGENCE") {
		t.Errorf("DiffResult.String (diverged) missing detail: %s", s)
	}
}

// TestCoverageSummaryRenderShort renders a fresh tracker's coverage summary,
// covering CoverageBucket.Exercised and CoverageSummary.String. A brand-new
// tracker has its scenario buckets registered-but-unexplored, so the summary
// reports unexplored buckets and the unexplored marker.
func TestCoverageSummaryRenderShort(t *testing.T) {
	t.Parallel()
	ct := NewCoverageTracker([]string{"alpha", "beta"})
	ct.Record(SwarmRun{Index: 0, Scenario: "alpha"})

	sum := ct.Summary()
	if sum.Exercised < 1 {
		t.Errorf("expected at least the recorded scenario exercised, got %d", sum.Exercised)
	}
	if sum.Unexplored < 1 {
		t.Errorf("expected at least one unexplored bucket, got %d", sum.Unexplored)
	}

	var sawExercised, sawUnexplored bool
	for _, b := range sum.Buckets {
		if b.Exercised() {
			sawExercised = true
		} else {
			sawUnexplored = true
		}
	}
	if !sawExercised || !sawUnexplored {
		t.Errorf("expected both exercised and unexplored buckets; exercised=%v unexplored=%v", sawExercised, sawUnexplored)
	}
	if s := sum.String(); !strings.Contains(s, "Coverage:") {
		t.Errorf("CoverageSummary.String missing header: %s", s)
	}
}

// TestOracleAccessorsShort covers the oracle's read accessors (HasNode, HasEdge,
// String) that the always-on per-tick checker reaches indirectly but no short
// test asserts directly. It drives the oracle through applyOpToOracle so the
// modelled state is real, then probes membership and the compact renderer.
func TestOracleAccessorsShort(t *testing.T) {
	t.Parallel()
	s := NewSeed(0xA11CE)
	oracle := NewGraphOracle()
	w := HonestWriter{}

	// Apply many honest writes so the oracle models nodes and (via the link
	// template) at least one edge.
	for i := 0; i < 200; i++ {
		op := w.NextOp(s, oracle)
		applyOpToOracle(oracle, op, true)
	}
	if oracle.NodeCount() == 0 {
		t.Fatal("expected the oracle to model at least one node after the honest writes")
	}

	// A never-used id is absent.
	if oracle.HasNode(^uint64(0)) {
		t.Error("HasNode(sentinel) = true for an id never created")
	}

	// Take a real modelled edge: its endpoints must report present and the edge
	// itself must be found, while a bogus label on the same endpoints is not.
	edges := oracle.edgeStates()
	if len(edges) == 0 {
		t.Fatal("expected at least one modelled edge after the honest writes")
	}
	e := edges[0]
	if !oracle.HasNode(e.SrcID) || !oracle.HasNode(e.DstID) {
		t.Errorf("HasNode = false for a modelled edge's endpoints (src=%d dst=%d)", e.SrcID, e.DstID)
	}
	if !oracle.HasEdge(e.SrcID, e.DstID, e.Label) {
		t.Errorf("HasEdge(%d,%d,%q) = false for a modelled edge", e.SrcID, e.DstID, e.Label)
	}
	if oracle.HasEdge(e.SrcID, e.DstID, "NO_SUCH_LABEL_9f8e") {
		t.Error("HasEdge for an absent label = true")
	}
	if s := oracle.String(); !strings.Contains(s, "GraphOracle{") {
		t.Errorf("GraphOracle.String missing prefix: %s", s)
	}
}
