package sim

import (
	"strings"
	"testing"
)

// TestSimReportNeverRendersEmptyShort pins the property whose absence made the
// 2026-08-07 ST3 durability sighting unactionable (rmp #2347): a report that
// carries a violation must NAME it, and a report that carries none must say so
// loudly instead of rendering a bare header an operator cannot distinguish from
// a clean run.
//
// The sighting itself was NOT caused by this renderer — the renderer cannot
// return an empty string, and the body was in fact discarded downstream by the
// gate's log filter (see scripts/failblocks.awk and scripts/test_failblocks.sh).
// This test exists so the renderer end of the same contract is pinned too,
// because the investigation had to establish it by reading rather than by
// running anything.
func TestSimReportNeverRendersEmptyShort(t *testing.T) {
	t.Parallel()

	t.Run("a violation is always named", func(t *testing.T) {
		t.Parallel()
		rep := &SimReport{
			Scenario: ScenarioCheckpointTeardown,
			Mode:     ModeConcurrent,
			Seed:     0xC0FFEE,
			Violations: []Violation{{
				Kind:    ViolationACIDDurability,
				Op:      "<recovery>",
				Message: "recovered 3 nodes < 5 acknowledged",
			}},
		}
		s := rep.String()
		if strings.TrimSpace(s) == "" {
			t.Fatal("a report carrying a violation rendered EMPTY")
		}
		// Every element an operator needs from a log line and nothing else.
		for _, want := range []string{
			string(ViolationACIDDurability),      // WHAT invariant broke
			"recovered 3 nodes < 5 acknowledged", // the offending observation
			ScenarioCheckpointTeardown,           // WHICH workload broke it
			"mode=concurrent",                    // and under which harness
			"Seed:",                              // how to try to replay it
			"Reproduce with:",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("rendered report is missing %q:\n%s", want, s)
			}
		}
	})

	t.Run("no violation is a loud reporting defect", func(t *testing.T) {
		t.Parallel()
		rep := &SimReport{Scenario: ScenarioCheckpointTeardown, Mode: ModeConcurrent, Seed: 1}
		s := rep.String()
		if !strings.Contains(s, "REPORTING DEFECT") {
			t.Errorf("a report with no violation must announce itself as a defect:\n%s", s)
		}
	})
}

// TestDurableReportPanicsWithoutViolationShort pins the fail-loud half of the
// same contract at the point of CONSTRUCTION. Every caller of durableReport
// reaches it under a len(v) > 0 guard; an empty slice therefore means the guard
// was bypassed, and the harness is about to declare a failure it cannot
// describe. That must cost a stack trace, not an investigation (rmp #2347).
func TestDurableReportPanicsWithoutViolationShort(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("durableReport with no violations must panic, not return a report that explains nothing")
		}
		if msg, _ := r.(string); !strings.Contains(msg, ScenarioCheckpointTeardown) {
			t.Errorf("panic message must name the scenario, got: %v", r)
		}
	}()
	_ = durableReport(ScenarioCheckpointTeardown, ModeConcurrent, 1, nil)
}
