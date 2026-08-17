package sim

import (
	"context"
	"strings"
	"testing"
)

// TestDDLCheckpointCrash_Scenario_Passes runs the registered scenario: a UNIQUE
// constraint and two indexes declared before a checkpoint that truncates the
// WAL prefix declaring them must survive the crash and be recovered from the
// snapshot — still enforcing, still answering seeks, still introspectable.
func TestDDLCheckpointCrash_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioDDLCheckpointCrash)
	if !ok {
		t.Fatalf("ddl-checkpoint-crash scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("ddl-checkpoint-crash run: %v", err)
	}
	if report != nil {
		t.Fatalf("ddl-checkpoint-crash reported a violation:\n%s", report)
	}
}

// TestDDLCheckpointCrash_NonVacuous is the measured-evidence gate: it asserts
// the run really did what the scenario claims, with numbers read off the
// durable image rather than inferred.
func TestDDLCheckpointCrash_NonVacuous(t *testing.T) {
	ev, report, err := runDDLCheckpointCrashWith(context.Background(),
		ddlCheckpointCrashScenario().DefaultSeed, defaultDDLCheckpointOptions())
	if err != nil {
		t.Fatalf("runDDLCheckpointCrashWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	if len(ev.cycles) != len(ddlCheckpointPhases()) {
		t.Fatalf("run completed %d phases, want %d", len(ev.cycles), len(ddlCheckpointPhases()))
	}
	if ev.checkpoints == 0 {
		t.Fatal("no checkpoint reclaimed WAL bytes: the snapshot path was never exercised")
	}
	var pure, tailed int
	for _, c := range ev.cycles {
		t.Logf("phase %-18s WAL: beforeDDL=%d afterDDL=%d beforeCP=%d afterCP=%d reclaimed=%d "+
			"walOpsReplayed=%d snapshot=%t dupRejected=%d freshAccepted=%d",
			c.name, c.walBeforeDDL, c.walAfterDDL, c.walBeforeCheckpoint, c.walAfterCheckpoint,
			c.reclaimed(), c.walOpsReplayed, c.snapshotPublished, c.dupRejected, c.freshAccepted)
		if !c.snapshotPublished {
			t.Errorf("phase %q published no snapshot manifest", c.name)
		}
		if c.walAfterDDL <= c.walBeforeDDL {
			t.Errorf("phase %q: the DDL appended no durable WAL bytes (%d -> %d)",
				c.name, c.walBeforeDDL, c.walAfterDDL)
		}
		if c.reclaimed() < c.walAfterDDL {
			t.Errorf("phase %q reclaimed %d WAL bytes but the DDL frames end at %d: the DDL survived in the WAL",
				c.name, c.reclaimed(), c.walAfterDDL)
		}
		if c.dupRejected == 0 || c.freshAccepted == 0 {
			t.Errorf("phase %q adjudicated %d rejections / %d acceptances, want both non-zero",
				c.name, c.dupRejected, c.freshAccepted)
		}
		if c.walAfterCheckpoint == 0 && c.walOpsReplayed == 0 {
			pure++
		}
		if c.walOpsReplayed > 0 {
			tailed++
		}
	}
	if pure == 0 {
		t.Error("no phase recovered from the snapshot ALONE (empty WAL, zero replayed ops)")
	}
	if tailed == 0 {
		t.Error("no phase recovered through the snapshot + WAL-tail path")
	}
}

// TestDDLCheckpointCrash_DetectsUntruncatedWAL is the FIRST sensitivity proof:
// with the checkpointer's constraint/index spec providers deliberately unwired
// — the pre-#1464/#1755 checkpointer — the published snapshot carries no
// constraints.bin/indexdefs.bin, the phase-3 self-sufficiency re-verification
// correctly refuses to truncate, and the run's "the DDL frames are gone from
// the WAL" oracle must FIRE.
//
// Without this the oracle could be asserting something the machinery makes
// true unconditionally; with it, the assertion is shown to depend on exactly
// the wiring the task exists to exercise.
func TestDDLCheckpointCrash_DetectsUntruncatedWAL(t *testing.T) {
	opts := defaultDDLCheckpointOptions()
	opts.wireSchemaSpecs = false

	ev, report, err := runDDLCheckpointCrashWith(context.Background(),
		ddlCheckpointCrashScenario().DefaultSeed, opts)
	if err != nil {
		t.Fatalf("runDDLCheckpointCrashWith: %v", err)
	}
	if report == nil {
		t.Fatal("a checkpoint published WITHOUT the schema specs passed silently:" +
			" the WAL-prefix reclamation oracle proves nothing")
	}
	if len(ev.cycles) == 0 {
		t.Fatal("no phase evidence was collected")
	}
	if got := ev.cycles[0].reclaimed(); got != 0 {
		t.Errorf("an unwired checkpoint reclaimed %d WAL bytes; the checkpointer was expected to REFUSE"+
			" to truncate a snapshot that cannot carry the schema", got)
	}
	if !violationMentions(report, "DDL WAL-prefix reclamation", "reclaimed 0 WAL bytes") {
		t.Fatalf("expected the WAL-prefix reclamation oracle to fire, got:\n%s", report)
	}
}

// TestDDLCheckpointCrash_DetectsSnapshotMissingIndexDefs is the SECOND
// sensitivity proof: recovery is fed a snapshot whose indexdefs.bin component
// has been removed after publication, so the recovered engine cannot carry the
// index definitions the WAL no longer holds either. The post-recovery oracles
// must FIRE (or recovery must fail-stop), never pass.
func TestDDLCheckpointCrash_DetectsSnapshotMissingIndexDefs(t *testing.T) {
	opts := defaultDDLCheckpointOptions()
	opts.damageSnapshot = func(disk *SimDisk, dir string) {
		_ = disk.Remove(dir + "/" + simSnapshotName + "/indexdefs.bin")
	}

	_, report, err := runDDLCheckpointCrashWith(context.Background(),
		ddlCheckpointCrashScenario().DefaultSeed, opts)
	switch {
	case err != nil:
		// The behaviour VERIFIED against this engine: recovery fail-stops on the
		// missing component rather than silently recovering a schema-less graph.
		// Pinned here so a future change to a silent-loss mode is caught.
		if !strings.Contains(err.Error(), "indexdefs.bin") {
			t.Fatalf("recovery failed for an unrelated reason: %v", err)
		}
		t.Logf("recovery fail-stopped on the damaged snapshot: %v", err)
	case report != nil:
		t.Logf("damaged-snapshot report:\n%s", report)
	default:
		t.Fatal("a snapshot missing indexdefs.bin recovered cleanly and every oracle stayed silent:" +
			" the post-recovery schema arms prove nothing")
	}
}

// TestDDLCheckpointCrash_NonVacuityGateWired proves the terminal
// assert-something-was-seen gate is really wired into the run rather than
// merely present: a DEGENERATE plan holding only the pure-snapshot phase
// exercises no snapshot+WAL-tail recovery, and the run must report that
// instead of passing.
func TestDDLCheckpointCrash_NonVacuityGateWired(t *testing.T) {
	opts := defaultDDLCheckpointOptions()
	opts.phases = ddlCheckpointPhases()[:1] // the pure-snapshot phase alone

	_, report, err := runDDLCheckpointCrashWith(context.Background(),
		ddlCheckpointCrashScenario().DefaultSeed, opts)
	if err != nil {
		t.Fatalf("runDDLCheckpointCrashWith: %v", err)
	}
	if report == nil {
		t.Fatal("a plan that never exercises the snapshot+WAL-tail path passed silently:" +
			" the non-vacuity gate is not wired")
	}
	if !violationMentions(report, "ddl-checkpoint non-vacuity", "snapshot + WAL-tail path") {
		t.Fatalf("expected the non-vacuity gate to name the missing WAL-tail arm, got:\n%s", report)
	}
}

// TestDDLCheckpointCrash_UniqueAdjudicatorDetectsBothArms is the sensitivity
// proof for the constraint accept/reject adjudicator: each arm must fire when
// the property it guards is broken. Without an ENFORCED constraint the
// duplicate arm must fire; with a held key passed as the "fresh" one the
// acceptance arm must fire — so neither arm can pass by accident.
func TestDDLCheckpointCrash_UniqueAdjudicatorDetectsBothArms(t *testing.T) {
	ctx := context.Background()

	t.Run("no constraint fires the duplicate arm", func(t *testing.T) {
		engine := ddlCheckpointFixture(ctx, t, false)
		accepted, rejected, vs := checkUniqueStillEnforced(ctx, 1, engine, ddlCheckpointKey(0), ddlCheckpointKey(99))
		if len(vs) == 0 {
			t.Fatal("the adjudicator FAILED to detect an unenforced UNIQUE constraint")
		}
		if rejected != 0 {
			t.Errorf("a duplicate was rejected with no constraint declared (rejected=%d)", rejected)
		}
		if accepted != 1 {
			t.Errorf("the fresh-key control did not commit (accepted=%d)", accepted)
		}
		if !containsViolation(vs, "recovered UNIQUE enforcement", "UNIQUE enforcement gap") {
			t.Errorf("expected an enforcement-gap violation, got %v", vs)
		}
	})

	t.Run("a held key as the fresh control fires the acceptance arm", func(t *testing.T) {
		engine := ddlCheckpointFixture(ctx, t, true)
		// Both keys are held, so the "fresh" CREATE is rejected too: the control
		// that proves the rejection discriminates is itself broken.
		_, _, vs := checkUniqueStillEnforced(ctx, 1, engine, ddlCheckpointKey(0), ddlCheckpointKey(1))
		if !containsViolation(vs, "recovered UNIQUE enforcement", "was REJECTED") {
			t.Fatalf("the adjudicator FAILED to report a rejected valid write; got %v", vs)
		}
	})
}

// TestDDLCheckpointCrash_SchemaModelDivergenceFires proves the recovered-schema
// arm the run calls is sensitive: dropping one declared index from the
// harness's model must make [ddlCheckpointAdjudicateSchema] fire.
func TestDDLCheckpointCrash_SchemaModelDivergenceFires(t *testing.T) {
	ctx := context.Background()
	engine := ddlCheckpointFixture(ctx, t, true)

	model := NewSchemaModel()
	model.AddUniqueConstraint(ddlCheckpointConstraintName, "Person", "key")
	model.AddIndex(ddlCheckpointCityIndexName, SchemaIndexHash, "Person", "city")
	specs := []IndexSpec{{Label: "Person", Property: "city"}, {Label: "Person", Property: "key"}}
	if v := ddlCheckpointAdjudicateSchema(1, model, engine, specs); len(v) > 0 {
		t.Fatalf("the schema arm fired on a faithful model: %v", v)
	}

	model.DropIndex(ddlCheckpointCityIndexName)
	if v := ddlCheckpointAdjudicateSchema(1, model, engine, specs); len(v) == 0 {
		t.Fatal("the schema arm FAILED to detect an index the model no longer declares")
	}
	model.AddIndex(ddlCheckpointCityIndexName, SchemaIndexHash, "Person", "city")
	model.DropConstraint(ddlCheckpointConstraintName)
	if v := ddlCheckpointAdjudicateSchema(1, model, engine, specs); len(v) == 0 {
		t.Fatal("the schema arm FAILED to detect a constraint the model no longer declares")
	}
}

// ddlCheckpointFixture builds a small in-memory engine holding the scenario's
// data shape, with the UNIQUE constraint and the city index declared only when
// withDDL is true. It is the seam the adjudicator sensitivity tests drive.
func ddlCheckpointFixture(ctx context.Context, t *testing.T, withDDL bool) *EngineAdapter {
	t.Helper()
	sm, err := New(Config{Seed: 1, MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	if withDDL {
		for _, ddl := range []string{ddlCheckpointConstraintDDL, ddlCheckpointCityIndexDDL} {
			if err := engineRunDDLOn(ctx, sm.engine, ddl); err != nil {
				t.Fatalf("DDL %q: %v", ddl, err)
			}
		}
	}
	written := 0
	if err := ddlCheckpointWriteBatch(ctx, sm.engine, NewSeed(1), &written, 8); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	return sm.engine
}

// containsViolation reports whether vs holds a violation with the given Op
// whose message contains substr.
func containsViolation(vs []Violation, op, substr string) bool {
	for _, v := range vs {
		if v.Op == op && strings.Contains(v.Message, substr) {
			return true
		}
	}
	return false
}

// violationMentions is [containsViolation] over a report.
func violationMentions(report *SimReport, op, substr string) bool {
	return report != nil && containsViolation(report.Violations, op, substr)
}
