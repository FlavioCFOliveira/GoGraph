package tck_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/tck"
)

// TestLoadWriteScenarios verifies that LoadWriteScenarios returns a
// meaningful number of scenarios from the write-clause feature directories,
// including scenarios expanded from Scenario Outline blocks.
func TestLoadWriteScenarios(t *testing.T) {
	ss, err := tck.LoadWriteScenarios()
	if err != nil {
		t.Fatalf("LoadWriteScenarios: %v", err)
	}
	total := len(ss)
	var skipped, run int
	for _, s := range ss {
		if s.SkipReason != tck.SkipNone {
			skipped++
		} else {
			run++
		}
	}
	t.Logf("write scenarios: total=%d run=%d skipped=%d", total, run, skipped)
	if total < 249 {
		t.Errorf("expected >= 249 write scenarios, got %d", total)
	}
}

// TestScenarioOutlineExpansion validates that Scenario Outline expansion
// increases the total scenario count beyond the pre-expansion baseline of
// 1615 and that the overall parser pass rate does not regress below the
// 67.6 % baseline recorded before expansion was introduced.
func TestScenarioOutlineExpansion(t *testing.T) {
	ss, err := tck.LoadScenarios()
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}

	// After expansion the total must exceed the pre-expansion baseline of 1615.
	total := len(ss)
	t.Logf("total scenarios after expansion: %d", total)
	if total <= 1615 {
		t.Errorf("expected more than 1615 scenarios after outline expansion, got %d", total)
	}

	// Compute the overall cumulative pass rate.
	var run, pass int
	for _, s := range ss {
		if s.SkipReason == tck.SkipNone {
			run++
			_, err := parser.Parse(s.Query)
			ok := (s.WantParseError() && err != nil) || (!s.WantParseError() && err == nil)
			if ok {
				pass++
			}
		}
	}
	overallPct := percent(pass, total)
	t.Logf("after expansion: total=%d run=%d pass=%d overall_rate=%.1f%%",
		total, run, pass, overallPct)

	// The cumulative pass rate must not regress below the pre-expansion baseline.
	const baselinePct = 67.6
	if overallPct < baselinePct {
		t.Errorf("overall pass rate regressed below baseline %.1f%%, got %.1f%%",
			baselinePct, overallPct)
	}
}
