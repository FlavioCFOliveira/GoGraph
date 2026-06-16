package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// alwaysFailScenario builds a scenario whose custom run always reports an
// invariant violation, so a swarm over it must surface every seed as a failure
// with a reproduce line. It is the integration fixture proving the swarm
// aggregates and reports failures (the swarm exists to FIND bugs).
func alwaysFailScenario(name string) Scenario {
	return Scenario{
		Name:        name,
		Description: "integration fixture: always reports a violation",
		Mode:        ModeDeterministic,
		DefaultSeed: 1,
		MaxTicks:    10,
		run: func(_ context.Context, seed uint64) (*SimReport, error) {
			return &SimReport{
				Seed:     seed,
				FailedOp: Op{Kind: OpMatch, Cypher: "<injected>"},
				Violations: []Violation{{
					Kind:    ViolationOracleDeviation,
					Op:      "<injected>",
					Message: "integration fixture forced failure",
				}},
			}, nil
		},
	}
}

// TestPhase5_SwarmAggregatesFailures runs a swarm over an always-failing
// scenario and asserts every run is surfaced as a failure with a reproduce line,
// in deterministic run-index order.
func TestPhase5_SwarmAggregatesFailures(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := NewRegistry(alwaysFailScenario("fail-fixture"), miniScenario("ok-fixture", 9, 20))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const runs = 12
	sw, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 0x1234, Scenario: "fail-fixture", Workers: 4, Runs: runs,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	res, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Swarm.Run: %v", err)
	}
	if res.FailureCount() != runs {
		t.Fatalf("expected all %d runs to fail, got %d failures", runs, res.FailureCount())
	}
	if res.Passes != 0 {
		t.Errorf("expected 0 passes, got %d", res.Passes)
	}
	// Failures are sorted by run index; the summary carries every reproduce line.
	for i := 1; i < len(res.Failures); i++ {
		if res.Failures[i-1].Index > res.Failures[i].Index {
			t.Errorf("failures not in run-index order: %d before %d", res.Failures[i-1].Index, res.Failures[i].Index)
		}
	}
	summary := res.Summary()
	if !strings.Contains(summary, "go run ./cmd/sim -scenario=fail-fixture") {
		t.Errorf("summary missing reproduce line:\n%s", summary)
	}
}

// TestPhase5_EndToEnd ties the Phase 5 surface together on one fast path: a
// swarm bracketed by the metrics oracle (goroutine baseline), feeding a coverage
// tracker, over a correct scenario — every piece cooperating with no leak.
func TestPhase5_EndToEnd(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	tracker := NewCoverageTracker(reg.Names())
	sw, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 0x5EED, Scenario: ScenarioReadHeavy, Workers: 4, Runs: 20,
		Observe: tracker.Record,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	res, mres, err := RunSwarmWithMetricsOracle(context.Background(), sw, 4)
	if err != nil {
		t.Fatalf("RunSwarmWithMetricsOracle: %v", err)
	}
	if res.FailureCount() != 0 {
		t.Fatalf("correct scenario swarm had failures:\n%s", res.Summary())
	}
	if !mres.Consistent() {
		t.Errorf("metrics goroutine-baseline bound violated:\n%s", mres.String())
	}
	if tracker.ScenarioCoverage()[ScenarioReadHeavy] != 20 {
		t.Errorf("coverage tracker did not record all 20 runs: %v", tracker.ScenarioCoverage())
	}
}

// TestPhase5_SoakSwarm is the soak-layer endurance swarm: a larger, longer
// bounded swarm over a correct deterministic scenario, asserting no failure and
// no goroutine leak across the whole run. It skips cleanly outside the soak
// layer (the only sanctioned skip).
func TestPhase5_SoakSwarm(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sw, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 0x50A4,
		Scenario:   ScenarioWriteHeavy,
		Workers:    8,
		Duration:   90 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	res, mres, err := RunSwarmWithMetricsOracle(context.Background(), sw, 8)
	if err != nil {
		t.Fatalf("soak swarm: %v", err)
	}
	if res.Runs == 0 {
		t.Fatal("soak swarm executed no runs")
	}
	if res.FailureCount() != 0 {
		t.Fatalf("soak swarm surfaced failures (investigate each reproduce line):\n%s", res.Summary())
	}
	if !mres.Consistent() {
		t.Errorf("soak swarm metrics/goroutine bound violated:\n%s", mres.String())
	}
	t.Logf("soak swarm: %s", res.Summary())
}
