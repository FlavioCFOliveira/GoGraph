package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestRun_PassesWithSeed verifies a small fixed-seed run exits 0 and prints the
// success summary including the seed.
func TestRun_PassesWithSeed(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"42", "--ticks=100"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Simulation passed. Seed: 42") {
		t.Fatalf("missing success line, got %q", out.String())
	}
}

// TestRun_InvalidSeed verifies a non-numeric seed argument exits 2.
func TestRun_InvalidSeed(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"not-a-number"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "invalid seed") {
		t.Fatalf("missing error message, got %q", errBuf.String())
	}
}

// TestRun_UnknownWorkload verifies an unknown workload name exits 2.
func TestRun_UnknownWorkload(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--workload=bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRun_Verbose verifies the verbose flag prints a per-op trace and the
// header.
func TestRun_Verbose(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"7", "--ticks=20", "--verbose"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "Running simulation:") {
		t.Fatalf("missing verbose header, got %q", s)
	}
	if !strings.Contains(s, "tick=") {
		t.Fatalf("missing per-op trace, got %q", s)
	}
}

// TestRun_WorkloadNames verifies each accepted workload name runs cleanly.
func TestRun_WorkloadNames(t *testing.T) {
	for _, name := range []string{"default", "write-heavy", "read-heavy"} {
		t.Run(name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := run([]string{"3", "--ticks=80", "--workload=" + name}, &out, &errBuf); code != 0 {
				t.Fatalf("%s: exit %d stderr=%q", name, code, errBuf.String())
			}
		})
	}
}

// TestRun_WireMode verifies the Phase-3 lock-step wire mode runs and reports a
// reproducible round-trip.
func TestRun_WireMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"42", "--mode=wire"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Wire round-trip reproducible") {
		t.Fatalf("missing wire success line, got %q", out.String())
	}
}

// TestRun_ConcurrentMode verifies the Phase-3 concurrent mode runs and reports a
// consistent quiescence.
func TestRun_ConcurrentMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"7", "--mode=concurrent", "--conns=8", "--ops-per-conn=10"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Concurrent run consistent") {
		t.Fatalf("missing concurrent success line, got %q", out.String())
	}
}

// TestRun_LivenessMode verifies the Phase-3 two-phase safety->liveness mode runs
// and converges.
func TestRun_LivenessMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"9", "--mode=liveness", "--conns=8", "--ops-per-conn=10"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "converged") {
		t.Fatalf("missing liveness success line, got %q", out.String())
	}
}

// TestRun_ListScenarios verifies -list-scenarios prints the catalogue and exits
// 0 without needing a seed.
func TestRun_ListScenarios(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"--list-scenarios"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "Scenario catalogue") {
		t.Fatalf("missing catalogue header, got %q", s)
	}
	for _, name := range []string{"crash-storm", "write-heavy", "schema-chaos", "bulk-vs-online", "long-running"} {
		if !strings.Contains(s, name) {
			t.Fatalf("catalogue missing %q:\n%s", name, s)
		}
	}
}

// TestRun_ScenarioMode verifies -scenario runs a named deterministic scenario
// for an explicit seed and reports a pass.
func TestRun_ScenarioMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"42", "--scenario=write-heavy"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `Scenario "write-heavy" passed`) {
		t.Fatalf("missing scenario pass line, got %q", out.String())
	}
}

// TestRun_UnknownScenario verifies an unknown scenario name exits 2.
func TestRun_UnknownScenario(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--scenario=nope"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown scenario") {
		t.Fatalf("missing error, got %q", errBuf.String())
	}
}

// TestRun_ReplayClean verifies a plain -replay of a correct deterministic run
// passes (exit 0) and prints the full per-op trace.
func TestRun_ReplayClean(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"7", "--scenario=read-heavy", "--replay"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "Replaying seed=7") || !strings.Contains(s, "tick=") {
		t.Fatalf("missing replay trace, got %q", s)
	}
	if !strings.Contains(s, "Replay passed") {
		t.Fatalf("missing replay-pass line, got %q", s)
	}
}

// TestRun_ReplayWithInjectedFaultShrinks verifies the demo-fault replay exits 1
// and prints a shrunk minimal reproducer.
func TestRun_ReplayWithInjectedFaultShrinks(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"12345", "--replay", "--inject-demo-fault"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit %d, want 1; stderr=%q", code, errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "Minimal reproducer:") {
		t.Fatalf("missing minimal-reproducer line:\n%s", combined)
	}
	if !strings.Contains(combined, "FAULT:drop-engine-write") {
		t.Fatalf("minimal reproducer should retain the faulted op:\n%s", combined)
	}
}

// TestRun_ReplayRejectsNonDeterministicScenario verifies replaying a concurrent
// scenario is rejected (exit 2) because it is not bit-replayable.
func TestRun_ReplayRejectsNonDeterministicScenario(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--scenario=overload", "--replay"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit %d, want 2; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "not bit-replayable") {
		t.Fatalf("missing rejection message, got %q", errBuf.String())
	}
}

// TestRun_UnknownMode verifies an unknown mode exits 2.
func TestRun_UnknownMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--mode=bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown mode") {
		t.Fatalf("missing error, got %q", errBuf.String())
	}
}

// TestRun_Swarm verifies the -swarm flag runs a bounded, count-boxed swarm over
// a fast deterministic scenario, prints the summary, and exits 0 when every
// seed passes. It also confirms the leading-positional seed is honoured as the
// master seed alongside flags.
// TestRun_SwarmSmokeShort is the short-layer CLI swarm smoke: a tiny
// single-worker swarm via the CLI exits 0 and prints the swarm summary. It keeps
// the -swarm flag path wired on every PR; the larger multi-worker swarm runs in
// the soak lane.
func TestRun_SwarmSmokeShort(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"7", "--swarm", "--workers=1", "--runs=3", "--scenario=read-heavy"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "master-seed=7") || !strings.Contains(s, "runs=3") {
		t.Fatalf("missing swarm summary, got %q", s)
	}
	if !strings.Contains(s, "failures=0") {
		t.Fatalf("expected zero failures for a correct scenario, got %q", s)
	}
}

// TestRun_Swarm drives a multi-worker CLI swarm and asserts the summary.
//
// Gated to the soak layer: the 12-run multi-worker swarm over the read-heavy
// workload is one of the heaviest tests in the package under -race. The
// short-layer TestRun_SwarmSmokeShort covers the -swarm CLI path on every PR.
func TestRun_Swarm(t *testing.T) {
	testlayers.RequireSoak(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"7", "--swarm", "--workers=4", "--runs=12", "--scenario=read-heavy"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "master-seed=7") || !strings.Contains(s, "runs=12") {
		t.Fatalf("missing swarm summary, got %q", s)
	}
	if !strings.Contains(s, "failures=0") {
		t.Fatalf("expected zero failures for a correct scenario, got %q", s)
	}
}

// TestRun_SwarmCoverageReport verifies -coverage-report with -swarm prints the
// coverage summary including the unexplored buckets and the unobservable-signal
// disclosure.
//
// Gated to the soak layer: it runs a multi-worker swarm to populate the
// coverage report, which is heavy under -race. The short-layer
// TestRun_CoverageReportAlone covers the coverage-report rendering (template +
// unobservable-signal disclosure) without a swarm on every PR.
func TestRun_SwarmCoverageReport(t *testing.T) {
	testlayers.RequireSoak(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"7", "--swarm", "--workers=2", "--runs=8", "--scenario=read-heavy", "--coverage-report"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "Coverage:") || !strings.Contains(s, "read-heavy") {
		t.Fatalf("missing coverage report, got %q", s)
	}
	if !strings.Contains(s, "Unobservable without a production hook") {
		t.Fatalf("missing unobservable-signal disclosure, got %q", s)
	}
}

// TestRun_CoverageReportAlone verifies -coverage-report without -swarm prints
// the coverage template (every scenario unexplored) and exits 0.
func TestRun_CoverageReportAlone(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"--coverage-report"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Coverage:") {
		t.Fatalf("missing coverage template, got %q", out.String())
	}
}

// TestRun_SwarmUnknownScenario verifies an unknown swarm scenario exits 2.
func TestRun_SwarmUnknownScenario(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--swarm", "--scenario=bogus", "--runs=4"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit %d, want 2; stderr=%q", code, errBuf.String())
	}
}
