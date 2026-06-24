package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMemPressure_Scenario_Passes runs the registered mem-pressure scenario and
// asserts the engine honours the bounded-resource / graceful-degradation
// contract: no panic, no invariant violation. Over-budget reads are refused with
// a typed error and change no state, so engine and oracle stay in lock-step.
func TestMemPressure_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioMemPressure)
	if !ok {
		t.Fatalf("mem-pressure scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("mem-pressure run: %v", err)
	}
	if report != nil {
		t.Fatalf("mem-pressure reported a violation (graceful degradation broke):\n%s", report)
	}
}

// TestMemPressure_NonVacuous asserts the run genuinely exercised the budgets:
// over-budget reads were rejected, while the honest writes still committed and
// the parity check stayed clean. A zero rejected-read count would mean the
// budgets never bit and the test proves nothing.
func TestMemPressure_NonVacuous(t *testing.T) {
	cfg := Config{
		Seed:     0x3E308E55,
		MaxTicks: 500,
		Workload: MemPressureWorkload(NewSeed(0x3E308E55)),
		EngineOpts: cypher.EngineOptions{
			MaxResultRows:   memPressureMaxResultRows,
			MaxCollectItems: memPressureMaxCollectItems,
		},
	}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	report, err := sm.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report != nil {
		t.Fatalf("mem-pressure reported a violation:\n%s", report)
	}
	if sm.RejectedReads() == 0 {
		t.Fatalf("vacuous run: no read was ever refused (0 rejected reads); the budgets never fired")
	}
	if sm.Oracle().NodeCount() == 0 {
		t.Fatalf("expected the honest writer to commit a non-empty graph alongside the rejected reads")
	}
	t.Logf("mem-pressure: rejectedReads=%d oracleNodes=%d edges=%d",
		sm.RejectedReads(), sm.Oracle().NodeCount(), sm.Oracle().EdgeCount())
}

// TestMemPressure_GracefulDegradation is the focused contract assertion: under a
// clamped row budget an over-budget read returns a TYPED error (not a panic, not
// a truncated-but-successful result), and — crucially — the engine is NOT wedged
// by the refusal: a subsequent in-budget write still commits and a subsequent
// in-budget read still succeeds. This is the "degrade, never fail catastrophically"
// guarantee.
func TestMemPressure_GracefulDegradation(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		MaxResultRows:   memPressureMaxResultRows,
		MaxCollectItems: memPressureMaxCollectItems,
	})
	ctx := context.Background()

	// An over-budget read must be refused with an error during the drain.
	res, err := eng.Run(ctx, "UNWIND range(1, 5000) AS x RETURN x", nil)
	if err == nil {
		// Streaming engines surface the budget breach during drain, not at Run.
		drained := 0
		for res.Next() {
			drained++
		}
		derr := res.Err()
		_ = res.Close()
		if derr == nil {
			t.Fatalf("over-budget read returned %d rows with no error; want a typed resource-exhausted error", drained)
		}
	} else {
		// Refused eagerly — also acceptable.
		t.Logf("over-budget read refused at Run: %v", err)
	}

	// The engine must NOT be wedged: a subsequent in-budget write commits.
	if _, werr := eng.RunInTx(ctx, "CREATE (:Person {name:'ok'})", nil); werr != nil {
		t.Fatalf("engine wedged after budget refusal: in-budget write failed: %v", werr)
	}
	// And a subsequent in-budget read succeeds.
	r2, rerr := eng.Run(ctx, "MATCH (n:Person) RETURN n.name", nil)
	if rerr != nil {
		t.Fatalf("in-budget read after refusal failed: %v", rerr)
	}
	rows := 0
	for r2.Next() {
		rows++
	}
	if err := r2.Err(); err != nil {
		t.Fatalf("in-budget read drain after refusal: %v", err)
	}
	_ = r2.Close()
	if rows != 1 {
		t.Fatalf("in-budget read returned %d rows, want 1 (the node created after the refusal)", rows)
	}
}
