package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestIndexDiversity_Scenario_Passes runs the index-diversity scenario: a hash
// (string), btree (numeric), and btree (string) index are created over an
// above-threshold graph (parallel backfill) and must stay seek-vs-scan
// consistent through write churn and every crash/recovery.
func TestIndexDiversity_Scenario_Passes(t *testing.T) {
	// Layer: soak. This scenario runs a ~9000-node above-threshold graph with
	// parallel index backfill through repeated crash/recovery; under `go test
	// -race` it is by far the most expensive test in the package. Soak-gating it
	// keeps the short layer inside the 60 s per-package SOFT budget that
	// scripts/pkg_time_budget.sh warns on (HARD_BUDGET defaults to 0, i.e.
	// disabled, so no hard ceiling is in force). Its numeric seek-vs-scan path is
	// covered in the short layer by the fast sibling
	// TestIndexConsistency_NumericBranch, so soak-gating the full-scale scenario
	// keeps the short layer under budget without losing short-layer coverage of
	// the checker path.
	testlayers.RequireSoak(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioIndexDiversity)
	if !ok {
		t.Fatalf("index-diversity scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("index-diversity run: %v", err)
	}
	if report != nil {
		t.Fatalf("index-diversity reported a violation (index inconsistency):\n%s", report)
	}
}

// indexDiversityShortBulk / indexDiversityShortTicks size the SHORT-layer slice
// of the index-diversity loop. The graph is deliberately BELOW the parallel
// backfill threshold — that arm is the soak-gated scenario's business — because
// what the short slice exists to prove is the checkpoint/crash WIRING (rmp
// #2464): that the run loop really publishes snapshots and really recovers
// through them.
const (
	indexDiversityShortBulk  = 3000
	indexDiversityShortTicks = 100
)

// TestIndexDiversity_CheckpointCrashWiredShort drives the SAME index-diversity
// loop at a short-layer budget and proves, with measured counters, that
// checkpointing is not merely configured but actually fires: a
// [CheckpointConfig] is inert unless the custom run loop calls
// [Simulator.maybeCheckpoint], which is exactly the trap rmp #2457 hit.
//
// A published checkpoint truncates the WAL prefix that declared the three
// indexes, so the crashes that follow recover their DEFINITIONS from the
// snapshot's indexdefs.bin rather than by replaying CREATE INDEX — the property
// the whole scenario now covers and did not before.
func TestIndexDiversity_CheckpointCrashWiredShort(t *testing.T) {
	sc := indexDiversityScenario()
	if !sc.Checkpoint.Enabled {
		t.Fatal("the index-diversity scenario no longer enables checkpointing")
	}
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = indexDiversityShortTicks
	// Crash more often than the full-scale scenario so the short budget still
	// contains crash/recovery cycles after a checkpoint.
	cfg.Crash.CrashProb = 1.0 / 25.0

	sm, report, err := runIndexDiversitySim(context.Background(), sc.DefaultSeed, cfg, indexDiversityShortBulk)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runIndexDiversitySim: %v", err)
	}
	if report != nil {
		t.Fatalf("index-diversity (short slice) reported a violation:\n%s", report)
	}
	t.Logf("index-diversity short slice: checkpoints=%d crashes=%d replayedWALOps=%d",
		sm.CheckpointCount(), sm.CrashCount(), sm.ReplayedOps())
	if sm.CheckpointCount() == 0 {
		t.Fatal("the run published NO checkpoint: the loop does not call maybeCheckpoint," +
			" so the index definitions never crossed the snapshot boundary")
	}
	if sm.CrashCount() == 0 {
		t.Fatal("the run never crashed: the post-checkpoint recovery path was never exercised")
	}
}

// TestIndexDiversity_CheckpointGateWired proves the terminal checkpoint
// non-vacuity gate is really wired into the index-diversity run rather than
// merely present: with checkpointing DISABLED the run must report the gate's
// violation instead of passing silently.
func TestIndexDiversity_CheckpointGateWired(t *testing.T) {
	sc := indexDiversityScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = indexDiversityShortTicks
	cfg.Checkpoint = CheckpointConfig{} // disabled: no snapshot can be published

	sm, report, err := runIndexDiversitySim(context.Background(), sc.DefaultSeed, cfg, indexDiversityShortBulk)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runIndexDiversitySim: %v", err)
	}
	if report == nil {
		t.Fatal("a run that published no checkpoint passed silently: the gate is not wired")
	}
	if !violationMentions(report, "checkpoint non-vacuity", "published NO checkpoint") {
		t.Fatalf("expected the checkpoint non-vacuity gate to fire, got:\n%s", report)
	}
}

// TestIndexConsistency_NumericBranch exercises the numeric (btree) branch of the
// consistency checker directly on a small graph (fast): the integer-keyed scan
// must group exactly like the integer-bound seek resolves. It guards the numeric
// scan/seek path the index-diversity scenario relies on without the cost of the
// 9000-node parallel backfill.
func TestIndexConsistency_NumericBranch(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := NewEngineAdapter(cypher.NewEngine(g))
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		q := "CREATE (:Person {name:'p', age:$age})"
		if _, err := eng.RunWrite(ctx, q, map[string]any{"age": int64(i % 7)}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := eng.RunWrite(ctx, "CREATE INDEX i_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", nil); err != nil {
		t.Fatalf("create numeric index: %v", err)
	}
	if v := CheckIndexConsistency(0, nil, eng, IndexSpec{Label: "Person", Property: "age", Numeric: true}); len(v) > 0 {
		t.Fatalf("numeric index inconsistent: %s", v[0].Message)
	}
}
