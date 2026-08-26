package sim

// report_repro_test.go — the "Reproduce with:" line must reproduce, or say it
// cannot (rmp #2621).
//
// It was always `go run ./cmd/sim <seed>`, unconditionally. That command runs
// cmd/sim's DEFAULT workload, so for a failure raised from a NAMED scenario it
// re-ran something else and reported SUCCESS. MEASURED on the #2620 soak
// failure: the report said `go run ./cmd/sim 2516635845` and that command
// printed "Simulation passed. Seed: 2516635845, Ticks: 100000", while
// `go run ./cmd/sim -scenario=production-profile 2516635845` runs the scenario
// the report names.
//
// A reproduction that passes is the most convincing lie an instrument can tell:
// it is evidence for the wrong conclusion, that the failure was a flake. This
// project has repeatedly been cost by an instrument that lied rather than one
// that was silent.

import (
	"strings"
	"testing"
)

// unqualifiedSimCommand is the shape that must never be printed for a run the
// seed does not determine.
const unqualifiedSimCommand = "go run ./cmd/sim "

// reproOf renders a report and returns the body of its "Reproduce with:" line.
func reproOf(t *testing.T, r *SimReport) string {
	t.Helper()
	for _, line := range strings.Split(r.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "Reproduce with: "); ok {
			return rest
		}
	}
	t.Fatalf("the report printed no Reproduce line at all:\n%s", r.String())
	return ""
}

func fabricatedReport(scenario string, seed uint64) *SimReport {
	return &SimReport{
		Scenario:   scenario,
		Mode:       ModeConcurrent,
		Seed:       seed,
		Violations: []Violation{{Kind: ViolationACIDConsistency, Message: "fabricated"}},
	}
}

// TestReproLine_DefaultWorkloadKeepsItsCommand is the "no existing reproducible
// case loses its line" half. A report with no scenario name IS the default
// workload, which the seed does determine.
func TestReproLine_DefaultWorkloadKeepsItsCommand(t *testing.T) {
	got := reproOf(t, fabricatedReport("", 42))
	if got != "go run ./cmd/sim 42" {
		t.Errorf("the default workload's reproduction line is %q, want %q; it was correct before "+
			"#2621 and must not be lost to the fix", got, "go run ./cmd/sim 42")
	}
}

// TestReproLine_CatalogueScenarioCarriesTheSelector pins the defect that made
// the line wrong for EVERY named scenario, not only test-configured ones: it
// omitted -scenario, so it re-ran the default workload.
func TestReproLine_CatalogueScenarioCarriesTheSelector(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if _, ok := reg.Lookup(ScenarioProductionProfile); !ok {
		t.Fatalf("%q is not in the catalogue, so this test is not exercising the branch it "+
			"exists for", ScenarioProductionProfile)
	}

	got := reproOf(t, fabricatedReport(ScenarioProductionProfile, 2516635845))
	want := "go run ./cmd/sim -scenario=" + ScenarioProductionProfile + " 2516635845"
	if got != want {
		t.Errorf("a catalogue scenario's reproduction line is %q, want %q. Without -scenario the "+
			"command runs cmd/sim's DEFAULT workload and reports success (#2621)", got, want)
	}
}

// TestReproLine_UnknownScenarioRefusesToPrintACommand is the core property: a
// run the seed alone does not determine must NOT be given an unqualified
// command.
func TestReproLine_UnknownScenarioRefusesToPrintACommand(t *testing.T) {
	got := reproOf(t, fabricatedReport("a-scenario-driven-only-from-a-test", 7))

	if strings.Contains(got, unqualifiedSimCommand) {
		t.Errorf("a report whose scenario is not in the catalogue printed a cmd/sim command "+
			"anyway: %q. That command runs a different workload and reports success, which is "+
			"evidence for the wrong conclusion (#2621)", got)
	}
	if !strings.Contains(got, "NOT REPRODUCIBLE") {
		t.Errorf("the line does not say the run cannot be reproduced from the seed: %q. A report "+
			"with no reproduction is strictly better than one whose line passes, but only if it "+
			"says so", got)
	}
}

// TestReproLine_ExplicitReproIsPrintedVerbatim covers the test-configured case:
// the caller supplies the Go call, because the configuration has no
// command-line form.
func TestReproLine_ExplicitReproIsPrintedVerbatim(t *testing.T) {
	size := productionProfileConfig{connections: 256, opsPerConn: 12, cycles: 3, counters: 4}
	r := fabricatedReport(ScenarioProductionProfile, 0x9600D0C5)
	r.Repro = size.reproCall(0x9600D0C5)

	got := reproOf(t, r)
	if strings.Contains(got, unqualifiedSimCommand) {
		t.Errorf("an explicitly-supplied reproduction was overridden by a cmd/sim command: %q", got)
	}
	for _, want := range []string{
		"runProductionProfile", "connections: 256", "opsPerConn: 12", "cycles: 3", "counters: 4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the reproduction line omits %q, so it does not name the configuration that "+
				"failed: %q", want, got)
		}
	}
	// MEASURED: with the #2620 oracle defect reintroduced, this exact call is
	// what TestProductionProfile_SoakFullScale runs, and it fails. The line was
	// verified by RUNNING it, not by reading it — which is the failure mode this
	// whole task is about.
}

// TestReproLine_EveryCatalogueScenarioIsReproducible sweeps the catalogue, so a
// scenario added later cannot quietly acquire a line that reports success.
func TestReproLine_EveryCatalogueScenarioIsReproducible(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	names := reg.Names()
	if len(names) == 0 {
		t.Fatal("the catalogue is empty, so this sweep asserts nothing")
	}
	for _, name := range names {
		got := reproOf(t, fabricatedReport(name, 1234))
		if !strings.Contains(got, "-scenario="+name) {
			t.Errorf("scenario %q renders %q, which does not name it. Every catalogue scenario "+
				"must be reproducible through -scenario, or its report sends a reader to a "+
				"different workload (#2621)", name, got)
		}
	}
	t.Logf("%d catalogue scenarios all render a -scenario selector", len(names))
}
