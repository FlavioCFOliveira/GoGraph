package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// TestPhase4_RecordReplayShrink_OverCatalogue is the Phase-4 end-to-end
// integration: for each DETERMINISTIC catalogue scenario it (a) records the
// trace, (b) confirms a clean recording replays to a clean result, then (c)
// injects a lost-write fault into the recorded trace and confirms the shrinker
// reduces it to a small reproducer that still fails. It ties recording, scripted
// replay, and ddmin shrinking together on the real scenario op streams.
func TestPhase4_RecordReplayShrink_OverCatalogue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}

	for _, sc := range reg.Scenarios() {
		if !sc.Mode.Reproducible() {
			continue // recording/replay/shrink apply to deterministic scenarios only
		}
		sc := sc
		// A small, fast slice of each deterministic scenario for the integration loop.
		cfg := sc.DeterministicConfig(sc.DefaultSeed)
		cfg.MaxTicks = 200
		cfg.Crash = CrashConfig{} // disable crashes: scripted replay targets the no-crash path

		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			trace, report, err := RecordTrace(ctx, cfg)
			if err != nil {
				t.Fatalf("RecordTrace: %v", err)
			}
			if report != nil {
				t.Fatalf("clean recording unexpectedly failed:\n%s", report)
			}

			// A clean trace replays cleanly.
			clean, err := ReplayTrace(ctx, trace)
			if err != nil {
				t.Fatalf("ReplayTrace clean: %v", err)
			}
			if clean.Violated() {
				t.Fatalf("clean trace replayed to a violation:\n%s", clean.Report)
			}

			// Inject a lost-write fault on a CREATE op deep in the trace, then shrink.
			faulted, ok := injectFaultOnCreate(trace)
			if !ok {
				t.Skipf("scenario %q produced no CREATE op to fault (workload-shaped); recording+replay still verified", sc.Name)
			}
			res, err := ShrinkTrace(ctx, faulted, ShrinkConfig{})
			if err != nil {
				t.Fatalf("ShrinkTrace: %v", err)
			}
			if res.MinimalLen >= res.OriginalLen {
				t.Fatalf("no reduction: %d -> %d", res.OriginalLen, res.MinimalLen)
			}
			// The minimal trace must still reproduce a violation.
			again, err := ReplayTrace(ctx, res.Minimal)
			if err != nil {
				t.Fatalf("replay minimal: %v", err)
			}
			if !again.Violated() {
				t.Fatal("minimal trace no longer reproduces the violation")
			}
		})
	}
}

// injectFaultOnCreate returns a copy of trace with a lost-write fault on the
// first CREATE op past its midpoint (so the failing op is buried), and whether a
// CREATE op was available.
func injectFaultOnCreate(trace Trace) (Trace, bool) {
	ops := make([]TracedOp, len(trace.Ops))
	copy(ops, trace.Ops)
	for i := len(ops) / 2; i < len(ops); i++ {
		if ops[i].Op.Kind == OpCreate {
			ops[i].Fault = FaultDropEngineWrite
			return trace.withOps(ops), true
		}
	}
	return trace, false
}

// TestPhase4_IndexConsistency_CatchesAndPasses is the Phase-4 index-consistency
// integration: the schema-chaos scenario (DDL churn + index-consistency check)
// passes on a correct run, and an injected divergence is caught.
func TestPhase4_IndexConsistency_CatchesAndPasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Correct run after DDL churn: schema-chaos passes.
	sc := schemaChaosScenario()
	sc.MaxTicks = 300
	report, err := sc.Run(ctx, sc.DefaultSeed)
	if err != nil {
		t.Fatalf("schema-chaos run: %v", err)
	}
	if report != nil {
		t.Fatalf("schema-chaos reported a violation on a correct run:\n%s", report)
	}

	// Injected divergence: a divergent fake engine trips the consistency check.
	fe := &divergentIndexEngine{
		scan: []scanRow{{id: 1, val: "A"}},
		seek: map[string][]int64{"A": {1, 2}},
	}
	if v := CheckIndexConsistency(1, nil, fe, IndexSpec{Label: "Person", Property: "name"}); len(v) == 0 {
		t.Fatal("expected the injected index divergence to be caught")
	}
}

// TestPhase4_ConcurrentScenarios_NoLeak runs the concurrent and bulk-vs-online
// scenarios under goleak to assert no goroutine outlives the run.
func TestPhase4_ConcurrentScenarios_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	for _, name := range []string{ScenarioOverload, ScenarioBulkVsOnline} {
		sc, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("scenario %q missing", name)
		}
		report, err := sc.Run(ctx, sc.DefaultSeed)
		if err != nil {
			t.Fatalf("scenario %q: %v", name, err)
		}
		if report != nil {
			t.Fatalf("scenario %q reported a violation:\n%s", name, report)
		}
	}
}
