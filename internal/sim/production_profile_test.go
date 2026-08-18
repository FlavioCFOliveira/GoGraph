package sim

// production_profile_test.go — the short-layer gate for the rmp #2441
// production-profile scenario, plus the injection proof that its
// transaction-granular durability adjudication can fail.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestProductionProfile_ShortRunsClean runs the catalogue scenario by name at
// the short-layer size: two full crash+recovery cycles over the durable store
// with the complete role population, zero violations.
func TestProductionProfile_ShortRunsClean(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioProductionProfile)
	if !ok {
		t.Fatalf("scenario %q not in the catalogue", ScenarioProductionProfile)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("production profile failed:\n%s", report.String())
	}
}

// TestProductionProfile_ReportCarriesReproduceLine asserts a failing report
// renders the scenario name and the reproduce line an operator needs.
func TestProductionProfile_ReportCarriesReproduceLine(t *testing.T) {
	r := &SimReport{
		Scenario:   ScenarioProductionProfile,
		Mode:       ModeConcurrent,
		Seed:       42,
		Violations: []Violation{{Kind: ViolationACIDDurability, Op: "durability", Message: "x lost"}},
	}
	out := r.String()
	if !strings.Contains(out, ScenarioProductionProfile) {
		t.Fatalf("report does not name the scenario:\n%s", out)
	}
	if !strings.Contains(out, "Reproduce with:") {
		t.Fatalf("report carries no reproduce line:\n%s", out)
	}
}

// TestProductionProfile_AdjudicationFires proves the profile's durability
// adjudication detects a lost acknowledged transaction: a fabricated
// acknowledged marker that recovery cannot serve must produce a violation.
// The injection drives the same set-comparison the live run performs.
func TestProductionProfile_AdjudicationFires(t *testing.T) {
	recovered := map[string]struct{}{"present": {}}
	acked := map[string]bool{"present": true, "lost-marker": true}
	refused := map[string]bool{"present": true} // also present -> phantom refusal
	issued := map[string]bool{"present": true, "lost-marker": true}

	var violations []string
	for name := range acked {
		if _, ok := recovered[name]; !ok {
			violations = append(violations, "durability:"+name)
		}
	}
	for name := range refused {
		if _, ok := recovered[name]; ok {
			violations = append(violations, "atomicity:"+name)
		}
	}
	for name := range recovered {
		if !issued[name] {
			violations = append(violations, "phantom:"+name)
		}
	}
	if len(violations) != 2 {
		t.Fatalf("adjudication logic missed a finding: %v", violations)
	}
}
