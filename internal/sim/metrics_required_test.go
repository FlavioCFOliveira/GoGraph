package sim

// metrics_required_test.go — the wiring and the falsifiability proofs for the
// per-scenario required-counters declarations (rmp #2479).
//
// Each arm follows the standing structure rmp #2470/#2472 fixed:
//
//   - the SEPARATE shape-only non-vacuity gate runs FIRST
//     ([CheckCounterDeclShape]). It reads no run and asks only whether the
//     declaration could ever have failed;
//   - the VERDICT ([ScenarioCounterDecl.Check]) is unconditional;
//   - the WITNESS — what the run actually emitted — is logged with t.Logf and
//     never asserted, so a scheduling or seed detail cannot fail the run.
//
// None of these tests calls t.Parallel: they install the GLOBAL metrics sink (as
// the group-commit and db-teardown oracles do), so they must not run beside other
// metrics-emitting work.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// adjudicateCounterDecl runs the shape gate, then the verdict, then logs the
// witness. It is the shared body of every arm below.
func adjudicateCounterDecl(t *testing.T, decl ScenarioCounterDecl, obs MetricsObservation) {
	t.Helper()

	// Non-vacuity FIRST, and about the DECLARATION rather than about the run: a
	// declaration that could not have failed makes the verdict below meaningless
	// however the run went.
	if v := CheckCounterDeclShape(decl); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("declaration shape: %s", viol)
		}
		t.Fatalf("the declaration for %s proves nothing; the verdict below would be meaningless", decl.Scenario)
	}

	// The verdict, unconditional.
	for _, viol := range decl.Check(obs) {
		t.Errorf("required counters: %s", viol)
	}

	// Witness only.
	t.Logf("witness: %s", decl)
	t.Logf("witness: observed %s", obs)
}

// declFor returns the registered declaration for a scenario key, failing the test
// when the registry does not carry one — so a declaration deleted from
// [ScenarioCounterDecls] fails loudly instead of silently skipping its arm.
func declFor(t *testing.T, scenario string) ScenarioCounterDecl {
	t.Helper()
	for _, d := range ScenarioCounterDecls() {
		if d.Scenario == scenario {
			return d
		}
	}
	t.Fatalf("no required-counters declaration registered for %q", scenario)
	return ScenarioCounterDecl{}
}

// observeScenarioRun drives a registered [Scenario] at its default seed under the
// recording sink and returns what it emitted. The scenario's own report is
// LOGGED, not asserted: the scenario's verdict is already adjudicated by its own
// test, and this arm is about coverage.
func observeScenarioRun(t *testing.T, sc *Scenario) MetricsObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var report *SimReport
	obs, err := ObserveMetrics(func() error {
		var rerr error
		report, rerr = sc.Run(ctx, sc.DefaultSeed)
		return rerr
	})
	if err != nil {
		t.Fatalf("%s run error: %v", sc.Name, err)
	}
	if report != nil {
		t.Fatalf("%s reported a violation:\n%s", sc.Name, report)
	}
	return obs
}

// -----------------------------------------------------------------------------
// The declared arms
// -----------------------------------------------------------------------------

// TestMetricsRequiredCounters_ShapeGateHoldsForEveryDeclaration runs the
// shape-only non-vacuity gate over the WHOLE registry, so a declaration that
// could never fail is caught even if its own arm is not run.
func TestMetricsRequiredCounters_ShapeGateHoldsForEveryDeclaration(t *testing.T) {
	decls := ScenarioCounterDecls()
	if len(decls) == 0 {
		t.Fatal("the required-counters registry is empty")
	}
	seen := make(map[string]struct{}, len(decls))
	for _, d := range decls {
		if _, dup := seen[d.Scenario]; dup {
			t.Errorf("scenario %q is declared twice", d.Scenario)
		}
		seen[d.Scenario] = struct{}{}
		for _, v := range CheckCounterDeclShape(d) {
			t.Errorf("%s", v)
		}
		t.Logf("witness: %s", d)
	}
}

// TestMetricsRequiredCounters_CSRFilePublishFaultIsMetricsBlind adjudicates the
// ST4 declaration. It is the one arm that declares BLINDNESS rather than
// counters, and the assertion is that the blindness still holds: no name under
// "store." may be emitted while the atomic-publish faults fire. See
// [csrFilePublishFaultDecl] for why that is the honest declaration.
func TestMetricsRequiredCounters_CSRFilePublishFaultIsMetricsBlind(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := csrfilePublishFaultScenario()
	obs := observeScenarioRun(t, &sc)
	adjudicateCounterDecl(t, declFor(t, ScenarioCSRFilePublishFault), obs)
	t.Logf("witness: the arm emitted %d metric name(s) in total", len(obs.Names()))
}

// TestMetricsRequiredCounters_WALCorruption adjudicates the ST5 declaration: the
// interior-frame CRC failure must move the WAL decoder's error counter.
func TestMetricsRequiredCounters_WALCorruption(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := walCorruptionFailStopScenario()
	obs := observeScenarioRun(t, &sc)
	adjudicateCounterDecl(t, declFor(t, ScenarioWALCorruptionFailStop), obs)
}

// TestMetricsRequiredCounters_CheckpointDirFsyncFault adjudicates the ST6
// declaration: the failed prefix truncate AND the three-way poison signature it
// leaves on the writer.
func TestMetricsRequiredCounters_CheckpointDirFsyncFault(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := checkpointDirFsyncFaultScenario()
	obs := observeScenarioRun(t, &sc)
	adjudicateCounterDecl(t, declFor(t, ScenarioCheckpointDirFsyncFault), obs)
}

// TestMetricsRequiredCounters_CheckpointCrashStorm adjudicates the storm
// declaration: one aborted checkpoint per publish window, each failing inside the
// publish, and at least one interrupted-publish repair on the recovery side.
func TestMetricsRequiredCounters_CheckpointCrashStorm(t *testing.T) {
	defer goleak.VerifyNone(t)

	sc := checkpointCrashStormScenario()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var report *SimReport
	obs, err := ObserveMetrics(func() error {
		var rerr error
		report, rerr = runCheckpointCrashStorm(ctx, sc.DefaultSeed, shortCheckpointStorm())
		return rerr
	})
	if err != nil {
		t.Fatalf("runCheckpointCrashStorm: %v", err)
	}
	if report != nil {
		t.Fatalf("%s reported a violation:\n%s", sc.Name, report)
	}
	adjudicateCounterDecl(t, declFor(t, ScenarioCheckpointCrashStorm), obs)
}

// TestMetricsRequiredCounters_SnapshotCorruption adjudicates the snapshot-
// corruption declaration: the aggregates at one per component, plus the eight
// per-component decoder counters that say the damage was detected where it was
// done.
func TestMetricsRequiredCounters_SnapshotCorruption(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := snapshotCorruptionFailStopScenario()
	obs := observeScenarioRun(t, &sc)
	adjudicateCounterDecl(t, declFor(t, ScenarioSnapshotCorruptionFailStop), obs)

	// The mapper gap, logged so it stays visible rather than being inferred from
	// the declaration's silence. See [snapshotCorruptionDecl].
	t.Logf("witness: store.snapshot.ReadMapperString.errors = %d (the mapper arm has no per-component witness; it is covered only by the aggregates)",
		obs.Counter("store.snapshot.ReadMapperString.errors"))
}

// TestMetricsRequiredCounters_DBTeardownFaultOnClose adjudicates the db-teardown
// fault arm. [RunDBTeardown] installs the sink itself, so the observation is read
// off its evidence rather than through [ObserveMetrics] — nesting the two would
// clobber the inner sink.
func TestMetricsRequiredCounters_DBTeardownFaultOnClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunDBTeardown(context.Background(), DBTeardownConfig{
		Seed:         0x2479_FA0_17ED,
		Arm:          ArmDBTeardownConcurrentClosers,
		Closers:      dbTeardownClosers,
		FaultOnClose: true,
	})
	if err != nil {
		t.Fatalf("RunDBTeardown(fault-on-close): %v", err)
	}
	adjudicateCounterDecl(t, declFor(t, DeclDBTeardownFaultOnClose), ev.Counters)
}

// TestMetricsRequiredCounters_CheckpointCadenceTransientFault adjudicates the
// cadence transient-failure arm: exactly one periodic fire failing inside its
// snapshot publish.
func TestMetricsRequiredCounters_CheckpointCadenceTransientFault(t *testing.T) {
	defer goleak.VerifyNone(t)

	obs, err := ObserveMetrics(func() error {
		_, rerr := RunCheckpointCadence(context.Background(), CheckpointCadenceConfig{
			Seed:               0x2479_FA01_7ED,
			Arm:                ArmCheckpointCadenceTransientFault,
			FaultOnCadenceFire: true,
		})
		return rerr
	})
	if err != nil {
		t.Fatalf("RunCheckpointCadence(transient-fault): %v", err)
	}
	adjudicateCounterDecl(t, declFor(t, DeclCheckpointCadenceTransientFault), obs)
}

// -----------------------------------------------------------------------------
// Falsifiability: withdraw the fault and the declaration must fire
// -----------------------------------------------------------------------------

// requireDeclFires asserts that a declaration reports a violation naming every
// counter it requires. It is the shape a disable-a-fault proof takes: the point
// is not merely that SOMETHING failed, but that the specific declared counters
// are the ones reported missing.
func requireDeclFires(t *testing.T, decl ScenarioCounterDecl, obs MetricsObservation) {
	t.Helper()
	v := decl.Check(obs)
	if len(v) == 0 {
		t.Fatalf("%s: the declaration was SATISFIED with the fault withheld — it is not load-bearing (observed %s)",
			decl.Scenario, obs)
	}
	if len(v) != len(decl.Required) {
		t.Errorf("%s: %d of %d declared counters fired with the fault withheld; every one of them should have",
			decl.Scenario, len(v), len(decl.Required))
	}
	for _, viol := range v {
		t.Logf("witness: with the fault withheld, %s", viol)
	}
}

// TestMetricsRequiredCounters_WALCorruptionControlWithdrawsTheFault is the
// discriminator the ST5 declaration names, and the proof that the declaration is
// load-bearing.
//
// "store.wal.Decode.errors" is emitted for a BENIGN torn tail as well as for
// genuine corruption (store/wal/format.go raises it on the io.EOF path that
// yields [wal.ErrTornFrame]), so the counter moving does not by itself say the
// interior frame was corrupted. This control runs the IDENTICAL scenario with the
// byte flip withheld — same commits, same clean close, same clean and prefix
// replays, same reopen — and requires the counter to stay at ZERO. Nothing but
// the corruption can move it, and without the corruption the declaration fires.
//
// The run's own report is expected to be non-nil (the scenario's oracles correctly
// object that nothing was corrupted); this arm reads the metrics, not the verdict.
func TestMetricsRequiredCounters_WALCorruptionControlWithdrawsTheFault(t *testing.T) {
	defer goleak.VerifyNone(t)

	sc := walCorruptionFailStopScenario()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var report *SimReport
	obs, err := ObserveMetrics(func() error {
		var rerr error
		report, rerr = runWALCorruptionFailStopWith(ctx, sc.DefaultSeed, walCorruptionOptions{skipCorruption: true})
		return rerr
	})
	if err != nil {
		t.Fatalf("runWALCorruptionFailStopWith(skipCorruption): %v", err)
	}
	if report == nil {
		t.Fatal("the control run passed the scenario's own oracles with nothing corrupted: the run is not the control it claims to be")
	}
	if got := obs.Counter("store.wal.Decode.errors"); got != 0 {
		t.Errorf("store.wal.Decode.errors = %d with no corruption injected, want 0: something OTHER than the injected fault moves the counter the declaration relies on",
			got)
	}
	requireDeclFires(t, declFor(t, ScenarioWALCorruptionFailStop), obs)
}

// TestMetricsRequiredCounters_DBTeardownCleanArmWithdrawsTheFault is the second
// disable-a-fault proof, on a different scenario. The identical teardown with
// FaultOnClose withheld moves neither declared counter, so the declaration fires.
func TestMetricsRequiredCounters_DBTeardownCleanArmWithdrawsTheFault(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunDBTeardown(context.Background(), DBTeardownConfig{
		Seed:         0x2479_C1EA_11,
		Arm:          ArmDBTeardownConcurrentClosers,
		Closers:      dbTeardownClosers,
		FaultOnClose: false,
	})
	if err != nil {
		t.Fatalf("RunDBTeardown(clean control): %v", err)
	}
	if !ev.CloseErrNil {
		t.Fatalf("the control teardown published %q: it is not the clean arm it claims to be", ev.CloseErr)
	}
	requireDeclFires(t, declFor(t, DeclDBTeardownFaultOnClose), ev.Counters)
}

// TestMetricsRequiredCounters_CadenceCleanArmWithdrawsTheFault is the third
// disable-a-fault proof, and the discriminator the cadence declaration names. The
// clean arm runs the same geometry and the same commits with no fault; both
// declared counters stay at zero, so the declaration fires.
func TestMetricsRequiredCounters_CadenceCleanArmWithdrawsTheFault(t *testing.T) {
	defer goleak.VerifyNone(t)

	obs, err := ObserveMetrics(func() error {
		_, rerr := RunCheckpointCadence(context.Background(), CheckpointCadenceConfig{
			Seed: 0x2479_C1EA_11,
			Arm:  ArmCheckpointCadenceClean,
		})
		return rerr
	})
	if err != nil {
		t.Fatalf("RunCheckpointCadence(clean control): %v", err)
	}
	requireDeclFires(t, declFor(t, DeclCheckpointCadenceTransientFault), obs)
}
