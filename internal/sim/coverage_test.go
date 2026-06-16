package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// TestCoverageTracker_RecordAndSummary checks the tracker folds observable
// signals (scenario, outcome, failed op kind, violation classes) into the
// right buckets and reports exercised-vs-unexplored.
func TestCoverageTracker_RecordAndSummary(t *testing.T) {
	ct := NewCoverageTracker([]string{ScenarioReadHeavy, ScenarioWriteHeavy, ScenarioCrashStorm})

	// A clean pass on read-heavy.
	ct.Record(SwarmRun{Index: 0, Scenario: ScenarioReadHeavy})
	// A violation on write-heavy with a failed CREATE op and an oracle deviation.
	ct.Record(SwarmRun{
		Index:    1,
		Scenario: ScenarioWriteHeavy,
		Report: &SimReport{
			FailedOp:   Op{Kind: OpCreate},
			Violations: []Violation{{Kind: ViolationOracleDeviation}},
		},
	})

	sc := ct.ScenarioCoverage()
	if sc[ScenarioReadHeavy] != 1 || sc[ScenarioWriteHeavy] != 1 {
		t.Errorf("scenario coverage = %v, want read-heavy=1 write-heavy=1", sc)
	}
	if sc[ScenarioCrashStorm] != 0 {
		t.Errorf("crash-storm should be unexplored, got %d", sc[ScenarioCrashStorm])
	}

	sum := ct.Summary()
	if sum.Unexplored == 0 {
		t.Errorf("expected at least the crash-storm scenario bucket unexplored, summary:\n%s", sum.String())
	}
	// Verify the outcome + op-kind + violation dimensions got their hits.
	want := map[string]map[string]int{
		string(dimOutcome):   {outcomePass: 1, outcomeViolation: 1},
		string(dimOpKind):    {string(OpCreate): 1},
		string(dimViolation): {string(ViolationOracleDeviation): 1},
	}
	got := map[string]map[string]int{}
	for _, b := range sum.Buckets {
		if got[b.Dimension] == nil {
			got[b.Dimension] = map[string]int{}
		}
		got[b.Dimension][b.Key] = b.Count
	}
	for dim, kv := range want {
		for k, c := range kv {
			if got[dim][k] != c {
				t.Errorf("dim %s key %s = %d, want %d", dim, k, got[dim][k], c)
			}
		}
	}
}

// TestCoverageTracker_SelectBias asserts the selector steers toward the
// least-covered scenario: after recording heavy coverage on one scenario, the
// next Select returns one of the others.
func TestCoverageTracker_SelectBias(t *testing.T) {
	scenarios := []string{"a", "b", "c"}
	ct := NewCoverageTracker(scenarios)

	// Saturate "a"; "b" and "c" stay unexplored.
	for i := 0; i < 10; i++ {
		ct.Record(SwarmRun{Scenario: "a"})
	}
	got := ct.Select(0, "a")
	if got == "a" {
		t.Errorf("Select returned the saturated scenario %q; expected an under-covered one", got)
	}
	if got != "b" && got != "c" {
		t.Errorf("Select returned %q, want b or c", got)
	}
}

// TestCoverageBias_IncreasesRarePathCoverage is the AC test: a coverage-biased
// swarm exercises more distinct scenarios (rare paths) over an equal budget than
// a uniform (fixed-scenario) swarm. The biased run spreads runs across the whole
// catalogue universe; the uniform run pins every run to one scenario.
func TestCoverageBias_IncreasesRarePathCoverage(t *testing.T) {
	defer goleak.VerifyNone(t)

	// A registry of fast deterministic scenarios so the comparison is cheap and
	// the only varying factor is the selection policy.
	reg, err := NewRegistry(
		miniScenario("cov-a", 0x1, 40),
		miniScenario("cov-b", 0x2, 40),
		miniScenario("cov-c", 0x3, 40),
		miniScenario("cov-d", 0x4, 40),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const budget = 24

	// Uniform policy: every run pins to the single configured scenario.
	uniform := NewCoverageTracker(reg.Names())
	swU, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 7, Scenario: "cov-a", Workers: 4, Runs: budget,
		Observe: func(r SwarmRun) { uniform.Record(r) },
	})
	if err != nil {
		t.Fatalf("NewSwarm uniform: %v", err)
	}
	if _, err := swU.Run(context.Background()); err != nil {
		t.Fatalf("uniform run: %v", err)
	}

	// Biased policy: the tracker is also the selector, steering toward
	// under-covered scenarios. Feed it as it selects so the bias adapts.
	biased := NewCoverageTracker(reg.Names())
	swB, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 7, Scenario: "cov-a", Workers: 4, Runs: budget,
		Selector: biased,
		Observe:  func(r SwarmRun) { biased.Record(r) },
	})
	if err != nil {
		t.Fatalf("NewSwarm biased: %v", err)
	}
	if _, err := swB.Run(context.Background()); err != nil {
		t.Fatalf("biased run: %v", err)
	}

	distinct := func(ct *CoverageTracker) int {
		n := 0
		for _, c := range ct.ScenarioCoverage() {
			if c > 0 {
				n++
			}
		}
		return n
	}
	uDistinct := distinct(uniform)
	bDistinct := distinct(biased)

	if uDistinct != 1 {
		t.Errorf("uniform exercised %d distinct scenarios, want exactly 1 (pinned)", uDistinct)
	}
	if bDistinct <= uDistinct {
		t.Errorf("biased exercised %d distinct scenarios, want > uniform's %d (bias should reach rare paths)", bDistinct, uDistinct)
	}
	// With a budget >= the scenario universe and a least-covered bias, the biased
	// swarm should reach every scenario.
	if bDistinct != len(reg.Names()) {
		t.Errorf("biased exercised %d/%d scenarios; bias should cover the whole universe over this budget",
			bDistinct, len(reg.Names()))
	}
}

// TestCoverageTracker_UnobservableSignals pins the documented test/production
// boundary so a future change that silently drops the disclosure is caught.
func TestCoverageTracker_UnobservableSignals(t *testing.T) {
	ct := NewCoverageTracker(nil)
	got := ct.UnobservableSignals()
	want := map[string]bool{"cypher-exec-operators": true, "crashpoint-sites": true}
	if len(got) != len(want) {
		t.Fatalf("UnobservableSignals = %v, want keys %v", got, want)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected unobservable signal %q", s)
		}
	}
}

// miniScenario builds a tiny deterministic read-heavy scenario for the coverage
// comparison: small tick budget so a swarm of them runs well within the short
// layer. It carries no crash/DDL so it is a pure, fast, correct run.
func miniScenario(name string, seed uint64, ticks int) Scenario {
	return Scenario{
		Name:        name,
		Description: "coverage-bias test scenario",
		Mode:        ModeDeterministic,
		DefaultSeed: seed,
		MaxTicks:    ticks,
		Workload:    ReadHeavyWorkload,
	}
}
