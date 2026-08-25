package sim

import (
	"context"
	"fmt"
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

	ev := &indexDiversityEvidence{}
	sm, report, err := runIndexDiversitySim(context.Background(), sc.DefaultSeed, cfg, indexDiversityShortBulk, ev)
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

	sm, report, err := runIndexDiversitySim(context.Background(), sc.DefaultSeed, cfg, indexDiversityShortBulk,
		&indexDiversityEvidence{})
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

// TestIndexDiversity_HydrationAndIntersectShort is the measured proof that the
// two surfaces rmp #2490 added are really driven under simulation.
//
// One run of the short slice feeds both sub-assertions deliberately: the slice
// costs about a second under the race detector and this package is the one
// closest to the short layer's per-package budget, so a second identical run to
// assert a second surface would be pure waste. The sub-tests keep the failure
// attribution separate.
func TestIndexDiversity_HydrationAndIntersectShort(t *testing.T) {
	sc := indexDiversityScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = indexDiversityShortTicks
	cfg.Crash.CrashProb = 1.0 / 25.0

	ev := &indexDiversityEvidence{}
	sm, report, err := runIndexDiversitySim(context.Background(), sc.DefaultSeed, cfg, indexDiversityShortBulk, ev)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runIndexDiversitySim: %v", err)
	}
	if report != nil {
		t.Fatalf("index-diversity (short slice) reported a violation:\n%s", report)
	}

	// The deserialize-not-rebuild decision, asserted in BOTH directions. The two
	// outcomes are indistinguishable in every answer the engine gives — a
	// hydrated index must behave exactly as a rebuilt one — so the answers alone
	// can never tell which happened, and the engine-scoped population counter is
	// the only sound instrument. It is anchored to the number of indexes the
	// reopen registered rather than to a constant, so the assertion survives the
	// engine adding or removing an internal numeric companion.
	t.Run("hydration-arms", func(t *testing.T) {
		if ev.arms == nil {
			t.Fatal("the loop never constructed the hydration arms")
		}
		a := ev.arms
		t.Log(a.summary())

		if !a.applicable {
			t.Fatal("the arms reported themselves inapplicable although checkpointing is enabled:" +
				" the store was not opened full-stack")
		}
		// HYDRATE side: every registered index loaded from its snapshot payload,
		// and not one node reference backfilled.
		if !a.hydrateRan {
			t.Fatal("the hydrate arm never ran")
		}
		if a.hydrateRegistered == 0 {
			t.Fatal("the hydrate arm's reopen registered no index: its assertion would be vacuous")
		}
		if a.hydrateHydrated != a.hydrateRegistered || a.hydrateRebuilt != 0 {
			t.Fatalf("hydrate arm: hydrated=%d rebuilt=%d, want hydrated=%d rebuilt=0",
				a.hydrateHydrated, a.hydrateRebuilt, a.hydrateRegistered)
		}
		if a.hydrateBackfillled != 0 {
			t.Fatalf("hydrate arm backfilled %d node references although every index hydrated",
				a.hydrateBackfillled)
		}
		// The forced crossing's own measurement, which is the substrate-level
		// reason hydration was permitted: the checkpoint emptied the WAL and the
		// recovery replayed nothing out of it.
		if a.boundary.walAfter != 0 || a.boundary.walOpsReplayed != 0 {
			t.Fatalf("hydrate arm's crossing left WAL work behind (%s): the hydration precondition"+
				" was not actually established", a.boundary.summary())
		}
		// REFUSE side: a write to an indexed property after the checkpoint, so
		// every index must be rebuilt and the write must be reachable through them.
		if !a.staleRan {
			t.Fatal("the stale arm never ran")
		}
		if a.staleWALOps == 0 {
			t.Fatal("the stale arm's reopen replayed 0 WAL ops: the suffix was empty, so the" +
				" staleness gate was not the thing under test")
		}
		if a.staleRebuilt != a.staleRegistered || a.staleHydrated != 0 {
			t.Fatalf("stale arm: hydrated=%d rebuilt=%d, want hydrated=0 rebuilt=%d",
				a.staleHydrated, a.staleRebuilt, a.staleRegistered)
		}
		if a.staleBackfilled == 0 {
			t.Fatal("the stale arm reported every index rebuilt yet backfilled no node reference")
		}
		if !a.staleProbeSeen {
			t.Fatal("the stale arm never confirmed the post-checkpoint write was reachable through" +
				" the rebuilt indexes")
		}
		// Both sides must differ, or the pair proves nothing.
		if a.hydrateHydrated == a.staleHydrated {
			t.Fatalf("both arms reported the same hydrated count (%d): the two branches were not"+
				" distinguished", a.hydrateHydrated)
		}
	})

	// The intersect planner's bitmap composition — and with it its budgeted
	// RangeCount / RangeCountFrom gate — must really have been driven, and the
	// composed-plan marker must have been shown to discriminate a composition
	// from any other index seek.
	t.Run("intersect-probes", func(t *testing.T) {
		if ev.intersect == nil {
			t.Fatal("the loop never constructed the intersect probes")
		}
		composed, withRows, soloSeeks := ev.intersect.Counts()
		t.Logf("intersect probes: composed=%d withRows=%d soloSeeks=%d", composed, withRows, soloSeeks)
		if composed == 0 {
			t.Fatal("the planner never composed two indexes: the intersect path never ran")
		}
		if withRows == 0 {
			t.Fatal("no composed arm returned a row: every intersected comparison was between empty sets")
		}
		if soloSeeks == 0 {
			t.Fatal("no single-property control seeked without composing: nothing establishes that the" +
				" composed marker distinguishes an intersection")
		}
	})
}

// TestIndexIntersectProbes_MarkerDiscriminatesComposition pins the fact both the
// scenario's composed assertions and its solo control rest on: on this data
// shape a two-BTREE-property conjunction composes and a single-property
// predicate on the same property does NOT, through the same physical operator.
//
// It runs directly on a small in-memory engine so the fact is pinned without the
// cost of a full scenario slice, and it is the short-layer guard for the
// selectivity windows [NewIndexIntersectProbes] draws: a window that drifted
// outside the planner's per-conjunct ceiling would stop composing here.
func TestIndexIntersectProbes_MarkerDiscriminatesComposition(t *testing.T) {
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := NewEngineAdapter(cypher.NewEngine(g))
	// Above the planner's 1024-node label-population floor, with the same value
	// cycles the scenario's bulk load uses.
	const bulk = 1500
	for i := 0; i < bulk; i++ {
		q := fmt.Sprintf("CREATE (:Person {name:'p%d', age:%d, city:'c%d'})", i, i%500, i%100)
		if _, err := eng.RunWrite(ctx, q, nil); err != nil {
			t.Fatalf("bulk %d: %v", i, err)
		}
	}
	for _, ddl := range indexDiversityDDL {
		if err := engineRunDDLOn(ctx, eng, ddl); err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}

	k := NewIndexIntersectProbes(NewSeed(0x2490), bulk)
	if v := k.Check(0, eng); len(v) > 0 {
		t.Fatalf("intersect probes on a clean fixture reported:\n%s", violationsText(v))
	}
	composed, withRows, soloSeeks := k.Counts()
	if composed != 2 || withRows == 0 || soloSeeks != 1 {
		t.Fatalf("composed=%d withRows=%d soloSeeks=%d, want composed=2, withRows>0, soloSeeks=1"+
			" (both arms composing, the control seeking without composing)",
			composed, withRows, soloSeeks)
	}
	if v := k.Finish(0); len(v) > 0 {
		t.Fatalf("the non-vacuity gate fired on a run that composed both arms:\n%s", violationsText(v))
	}

	// The gate must FIRE on a checker that observed nothing, or its silence above
	// carries no information.
	if v := (&IndexIntersectProbes{}).Finish(0); len(v) != 3 {
		t.Fatalf("the non-vacuity gate reported %d violations for a checker that observed nothing,"+
			" want one per clause (composed, rows, solo control)", len(v))
	}
}

// TestNewIndexIntersectProbes_RejectsUnreachableFixture proves the probe
// constructor refuses a fixture below the planner's label-population floor
// rather than producing arms that assert a composition which cannot happen.
func TestNewIndexIntersectProbes_RejectsUnreachableFixture(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a sub-floor bulk size was accepted: every composed assertion would fail" +
				" for the wrong reason")
		}
	}()
	_ = NewIndexIntersectProbes(NewSeed(1), 512)
}
