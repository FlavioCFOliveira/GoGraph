package sim

// typed_schema_test.go — the short-layer gate for the typed-schema validator
// scenario (rmp #2493).
//
// The structure follows the standing shape of this package: the scenario runs
// clean, the evidence is asserted as NUMBERS (so "it passed" is separated from
// "it exercised something"), each non-vacuity gate is shown to be WIRED by
// driving a configuration that cannot satisfy it, and every clause is shown to
// be able to FAIL by a perturbation that reproduces the output the real defect
// would produce.
//
// The perturbation table is the load-bearing part. A clause whose silence has
// never been distinguished from its absence proves nothing, and this scenario's
// clauses are mostly of the form "nothing changed" — the easiest kind to satisfy
// accidentally.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestTypedSchema_ScenarioPasses runs the registered scenario at its catalogue
// seed and requires a clean report.
func TestTypedSchema_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioTypedSchema)
	if !ok {
		t.Fatalf("scenario %q is not registered", ScenarioTypedSchema)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("the typed-schema scenario reported a violation:\n%s", report)
	}
}

// TestTypedSchema_EvidenceIsNonVacuous asserts on the MEASURED numbers rather
// than on the absence of a violation.
//
// It duplicates the terminal gate deliberately: the gate fails the RUN, this
// test fails the BUILD with the numbers printed, and a budget change that
// quietly stops reaching an arm is far easier to diagnose from the second.
func TestTypedSchema_EvidenceIsNonVacuous(t *testing.T) {
	ev, report, err := RunTypedSchema(context.Background(),
		DefaultTypedSchemaConfig(typedSchemaDefaultSeed))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}

	for p := tsPath(0); p < tsPathCount; p++ {
		for v := tsVerdict(0); v < tsVerdictCount; v++ {
			if ev.Coverage[p][v] == 0 {
				t.Errorf("the (%s, %s) cell was never exercised", p, v)
			}
		}
		if ev.NoMutationChecks[p] == 0 {
			t.Errorf("no rejected write on %s had its no-mutation battery run", p)
		}
		if ev.AcceptLandedChecks[p] == 0 {
			t.Errorf("no accepted write on %s was read back", p)
		}
	}
	for v := tsNodeVerdict(0); v < tsNodeVerdictCount; v++ {
		if ev.NodeVerdicts[v] == 0 {
			t.Errorf("ValidateNode never returned %s", v)
		}
	}
	for v := tsVerdict(0); v < tsVerdictCount; v++ {
		if ev.EngineVerdicts[v] == 0 {
			t.Errorf("the durable Cypher path never produced %s", v)
		}
	}
	if ev.Crashes == 0 || ev.Checkpoints == 0 {
		t.Errorf("recovery coverage missing: crashes=%d checkpoints=%d", ev.Crashes, ev.Checkpoints)
	}
	if ev.WitnessReadsAfterRecovery == 0 {
		t.Errorf("no witness was verified on a recovered graph (armed=%d)", ev.WitnessesArmed)
	}
	if ev.PinNoValidatorAccepted == 0 || ev.PinReinstalledRejected == 0 ||
		ev.PinValidateNodeDetected == 0 || ev.PinValidateNodeRepaired == 0 {
		t.Errorf("the recovery pin did not reach every clause: %s", ev.String())
	}
	if ev.PureStoreArms == 0 || ev.PureStoreResurrected == 0 {
		t.Errorf("the pure store/txn arm did not run or did not observe the resurrection: %s", ev.String())
	}
	if ev.KeyInterningChecks == 0 || ev.CrossAccessorChecks == 0 || ev.FusedNoEdgeChecks == 0 {
		t.Errorf("a no-mutation sub-clause never ran: %s", ev.String())
	}
	t.Logf("typed-schema evidence: %s", ev.String())
}

// TestTypedSchema_IsDeterministic requires the same seed to produce the same
// evidence, twice. A harness that drifts makes every failure it ever reports
// unreplayable, so the claim is worth its own test.
func TestTypedSchema_IsDeterministic(t *testing.T) {
	cfg := DefaultTypedSchemaConfig(typedSchemaDefaultSeed)
	cfg.MaxTicks = 120

	first, report, err := RunTypedSchema(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("first run: err=%v report=%v", err, report)
	}
	second, report, err := RunTypedSchema(context.Background(), cfg)
	if err != nil || report != nil {
		t.Fatalf("second run: err=%v report=%v", err, report)
	}
	if first.ReproducibleSummary() != second.ReproducibleSummary() {
		t.Fatalf("the run is NOT reproducible:\nfirst:  %s\nsecond: %s",
			first.ReproducibleSummary(), second.ReproducibleSummary())
	}
	if first.Digest == 0 {
		t.Fatal("the digest is zero: nothing was folded into it, so it cannot witness reproducibility")
	}
}

// TestTypedSchema_CoverageGateFires proves the fifteen-cell coverage gate is
// really wired rather than merely present: a budget too small to complete one
// sweep epoch must REPORT the gate, not pass silently.
//
// It also pins the sweep's own claim. With a random draw per tick, a two-tick run
// would sometimes reach two cells and sometimes one, and the gate would fire
// either way for the wrong reason; with the sweep, exactly two cells are visited
// and thirteen are named in the report.
func TestTypedSchema_CoverageGateFires(t *testing.T) {
	cfg := DefaultTypedSchemaConfig(typedSchemaDefaultSeed)
	cfg.MaxTicks = 2

	_, report, err := RunTypedSchema(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report == nil {
		t.Fatal("a run that completed no sweep epoch passed silently: the coverage gate is not wired")
	}
	if !violationMentions(report, tsOp("vacuity:coverage"), "cell was never exercised") {
		t.Fatalf("expected the coverage gate to fire, got:\n%s", report)
	}
}

// TestTypedSchema_CheckpointGateFires proves the checkpoint non-vacuity gate is
// wired: with checkpointing disabled the run must report it instead of claiming a
// snapshot-crossing recovery it never produced.
func TestTypedSchema_CheckpointGateFires(t *testing.T) {
	sc := typedSchemaScenario()
	if !sc.Checkpoint.Enabled {
		t.Fatal("the typed-schema scenario no longer enables checkpointing")
	}
	cfg := DefaultTypedSchemaConfig(typedSchemaDefaultSeed)
	cfg.Checkpoint = CheckpointConfig{}

	_, report, err := RunTypedSchema(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report == nil {
		t.Fatal("a run that published no checkpoint passed silently: the gate is not wired")
	}
	if !violationMentions(report, tsOp("vacuity:checkpoints"), "published NO checkpoint") &&
		!violationMentions(report, tsOp("checkpoint-vacuity"), "published NO checkpoint") {
		t.Fatalf("expected a checkpoint non-vacuity gate to fire, got:\n%s", report)
	}
}

// TestTypedSchema_ForcedCrashSuppliesRecoveryCoverage proves the post-recovery
// coverage does not depend on the seeded crash schedule: with the schedule
// DISABLED the run must still reach the pin and the post-recovery witness read,
// through the crash the loop forces at the end.
func TestTypedSchema_ForcedCrashSuppliesRecoveryCoverage(t *testing.T) {
	cfg := DefaultTypedSchemaConfig(typedSchemaDefaultSeed)
	cfg.Crash = CrashConfig{}

	ev, report, err := RunTypedSchema(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}
	if ev.ForcedCrashes != 1 {
		t.Fatalf("ForcedCrashes = %d, want exactly 1 with the schedule disabled", ev.ForcedCrashes)
	}
	if ev.Crashes != 1 {
		t.Fatalf("Crashes = %d, want exactly 1 (the forced one)", ev.Crashes)
	}
	if ev.PinNoValidatorAccepted == 0 || ev.WitnessReadsAfterRecovery == 0 {
		t.Fatalf("the forced crash did not supply the recovery coverage: %s", ev.String())
	}
}

// -----------------------------------------------------------------------------
// Falsifiability — the side (direct-API) clauses
// -----------------------------------------------------------------------------

// newTypedSchemaSideFixture builds the side fixture and a probe set over a fixed
// seed. It is deliberately NOT the scenario: what these cases need is a graph
// with the schema installed and one chosen cell driven, reached in microseconds.
func newTypedSchemaSideFixture(t *testing.T, seed uint64) (*typedSchemaSide, *TypedSchemaProbes) {
	t.Helper()
	side, err := newTypedSchemaSide(typedSchemaSideNodes)
	if err != nil {
		t.Fatalf("newTypedSchemaSide: %v", err)
	}
	return side, NewTypedSchemaProbes(NewSeed(seed))
}

// TestTypedSchema_SideClausesAreFalsifiable drives one chosen (path, verdict)
// cell per case and requires the named clause to fire under the perturbation and
// to stay silent without it.
//
// Both directions are asserted. A perturbation that fires a clause proves the
// clause can fail; the unperturbed control beside it proves the clause is not
// simply always failing, which is the failure mode a one-directional
// falsifiability test cannot see.
func TestTypedSchema_SideClausesAreFalsifiable(t *testing.T) {
	cases := []struct {
		name    string
		path    tsPath
		want    tsVerdict
		perturb tsPerturb
		clause  string
	}{
		{"accept reported as reject", tsPathNodeProp, tsAccept, tsPerturbFlipVerdict, "verdict"},
		{"reject reported as accept", tsPathNodeProp, tsRejectTypeMismatch, tsPerturbFlipVerdict, "verdict"},
		{"rejected node write lands", tsPathNodeProp, tsRejectTypeMismatch, tsPerturbApplyRejected, "no-mutation:value"},
		{"rejected pair write lands", tsPathEdgePairProp, tsRejectTypeMismatch, tsPerturbApplyRejected, "no-mutation:value"},
		{"rejected handle write lands", tsPathEdgeHandleProp, tsRejectTypeMismatch, tsPerturbApplyRejected, "no-mutation:value"},
		{"rejected instance write lands", tsPathEdgeInstanceProp, tsRejectTypeMismatch, tsPerturbApplyRejected, "no-mutation:value"},
		{"rejected fused write lands", tsPathFusedAddEdge, tsRejectTypeMismatch, tsPerturbApplyRejected, "no-mutation:fused-edge"},
		{"unknown key is interned", tsPathNodeProp, tsRejectUnknownProperty, tsPerturbInternGhostKey, "no-mutation:key-interning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The control first, on its own fixture: the clause must be silent when
			// nothing is perturbed.
			side, probes := newTypedSchemaSideFixture(t, typedSchemaDefaultSeed)
			spec := tsSpecFor(tc.path, tc.want, probes.seed)
			if vs := probes.SideWrite(1, side, spec, tsPerturbNone); len(vs) > 0 {
				t.Fatalf("the UNPERTURBED control already failed, so the perturbation below proves "+
					"nothing:\n%v", vs)
			}
			// Then the perturbation, on a fresh fixture so the control's accepted
			// write cannot mask it.
			side, probes = newTypedSchemaSideFixture(t, typedSchemaDefaultSeed)
			spec = tsSpecFor(tc.path, tc.want, probes.seed)
			vs := probes.SideWrite(1, side, spec, tc.perturb)
			if len(vs) == 0 {
				t.Fatalf("perturbation %d produced NO violation: the %q clause cannot fail",
					tc.perturb, tc.clause)
			}
			if !tsViolationsMentionOp(vs, tsOp(tc.clause)) {
				t.Fatalf("expected the %q clause to fire, got:\n%v", tc.clause, vs)
			}
		})
	}
}

// TestTypedSchema_NodeClausesAreFalsifiable does the same for the ValidateNode
// boundary battery.
func TestTypedSchema_NodeClausesAreFalsifiable(t *testing.T) {
	cases := []struct {
		name    string
		perturb tsPerturb
		clause  string
	}{
		{"a complete node is reported mid-build", tsPerturbNodePrefill, "validate:mid-build"},
		{"a finalised node is missing its required property", tsPerturbNodeSkipRequired, "validate:finalised"},
		{"the pre-installation forbidden value is repaired", tsPerturbRepairPreInstall, "validate:pre-install"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			side, probes := newTypedSchemaSideFixture(t, typedSchemaDefaultSeed)
			if vs := probes.NodeBattery(1, side, tsPerturbNone); len(vs) > 0 {
				t.Fatalf("the UNPERTURBED control already failed:\n%v", vs)
			}
			side, probes = newTypedSchemaSideFixture(t, typedSchemaDefaultSeed)
			vs := probes.NodeBattery(1, side, tc.perturb)
			if len(vs) == 0 {
				t.Fatalf("perturbation %d produced NO violation: the %q clause cannot fail",
					tc.perturb, tc.clause)
			}
			if !tsViolationsMentionOp(vs, tsOp(tc.clause)) {
				t.Fatalf("expected the %q clause to fire, got:\n%v", tc.clause, vs)
			}
		})
	}
}

// TestTypedSchema_PreInstallFixtureIsTheOnlyRouteToTheKindRecheck pins WHY the
// pre-installation fixture exists.
//
// [schema.Schema.ValidateNode] re-checks the kinds of properties that are already
// PRESENT, a branch [schema.Schema.Validate] structurally cannot reach because it
// runs before the write. With the validator installed, no write can produce a
// forbidden stored value — so the branch is unreachable unless the value predates
// the installation (or arrives through the recovery bypass, which arm D covers).
// This test asserts that unreachability directly, so a future reader cannot
// mistake the fixture for redundant scaffolding.
func TestTypedSchema_PreInstallFixtureIsTheOnlyRouteToTheKindRecheck(t *testing.T) {
	t.Parallel()
	side, _ := newTypedSchemaSideFixture(t, typedSchemaDefaultSeed)

	// The forbidden value is present and ValidateNode sees it.
	if err := side.g.ValidateNode(tsPreInstallNode); err == nil {
		t.Fatal("the pre-installation fixture no longer carries a forbidden value")
	}
	// And it cannot be reproduced on a node built AFTER installation.
	const fresh = "ts-post-install"
	if err := side.g.AddNode(fresh); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := side.g.SetNodeProperty(fresh, tsKeyInt, lpg.StringValue("wrong-kind")); err == nil {
		t.Fatal("a wrong-kind write was ACCEPTED with the validator installed: the fixture's " +
			"pre-installation ordering is no longer necessary, and this test and the file header " +
			"both need revisiting")
	}
	if _, present := side.g.GetNodeProperty(fresh, tsKeyInt); present {
		t.Fatal("the refused write stored a value anyway")
	}
}

// -----------------------------------------------------------------------------
// Falsifiability — the durable clauses
// -----------------------------------------------------------------------------

// newTypedSchemaDurableFixture builds a durable simulator, runs the scenario's
// own prologue over it, and forces ONE crash+recovery, leaving the caller on a
// freshly recovered graph that carries no validator.
//
// The crash SCHEDULE is disabled on purpose: with it on, whether a crash lands
// inside a tiny budget is seed-dependent, and an arm only some seeds reach is an
// arm no test can pin. The forced crash is the deterministic route.
func newTypedSchemaDurableFixture(t *testing.T, seed uint64) (*Simulator, *TypedSchemaProbes) {
	t.Helper()
	cfg := DefaultTypedSchemaConfig(seed)
	cfg.MaxTicks = 0
	cfg.Crash = CrashConfig{}
	sm, err := New(typedSchemaSimConfig(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	probes := NewTypedSchemaProbes(NewSeed(seed ^ typedSchemaSideSeedMix))
	vs, err := typedSchemaPrologue(context.Background(), sm, probes)
	if err != nil {
		t.Fatalf("prologue: %v", err)
	}
	if len(vs) > 0 {
		t.Fatalf("prologue reported violations:\n%v", vs)
	}
	report, err := sm.forceCrash(1, tsOp("test-fixture-crash"))
	if err != nil {
		t.Fatalf("forced crash: %v", err)
	}
	if report != nil {
		t.Fatalf("the forced crash reported a durability violation:\n%s", report)
	}
	return sm, probes
}

// TestTypedSchema_RecoveryPinHoldsAndIsFalsifiable drives the post-recovery pin
// over a real forced recovery: unperturbed it must reach all five clauses, and
// with a validator pre-installed the first clause must fire.
//
// The perturbation is the whole point of the pin. "The recovered graph accepted
// the write" is a claim about an ABSENCE, and an absence claim that has never
// been shown to fail is indistinguishable from a clause that is not evaluated.
func TestTypedSchema_RecoveryPinHoldsAndIsFalsifiable(t *testing.T) {
	sm, probes := newTypedSchemaDurableFixture(t, typedSchemaDefaultSeed)
	if vs := probes.RecoveryPin(sm, 1, 1, tsPerturbNone); len(vs) > 0 {
		t.Fatalf("the recovery pin failed unperturbed:\n%v", vs)
	}
	ev := probes.Evidence()
	if ev.PinNoValidatorAccepted != 1 || ev.PinNoValidatorNodeClean != 1 ||
		ev.PinReinstalledRejected != 1 || ev.PinValidateNodeDetected != 1 ||
		ev.PinValidateNodeRepaired != 1 {
		t.Fatalf("the pin did not reach all five clauses exactly once: %s", ev.String())
	}

	sm2, probes2 := newTypedSchemaDurableFixture(t, typedSchemaDefaultSeed)
	vs := probes2.RecoveryPin(sm2, 1, 1, tsPerturbRecoveryPreinstall)
	if len(vs) == 0 {
		t.Fatal("a recovered graph WITH a validator installed satisfied the no-validator clause: " +
			"the clause cannot fail, so its silence in the scenario means nothing")
	}
	if !tsViolationsMentionOp(vs, tsOp("pin:no-validator")) {
		t.Fatalf("expected the pin:no-validator clause to fire, got:\n%v", vs)
	}
}

// TestTypedSchema_WitnessClauseIsFalsifiable drives the post-recovery witness
// read: unperturbed the accepted value must come back through both channels, and
// with the refused value planted the clause must fire.
//
// The plant reproduces the OUTPUT a validator-less WAL replay of the refused
// frame would produce, which is precisely the shape arm D2 measures on the pure
// store path.
func TestTypedSchema_WitnessClauseIsFalsifiable(t *testing.T) {
	sm, probes := newTypedSchemaDurableFixture(t, typedSchemaDefaultSeed)
	if len(probes.witnesses) == 0 {
		t.Fatal("the prologue armed no witness, so there is nothing to verify")
	}
	if vs := probes.RecoveryPin(sm, 1, 1, tsPerturbNone); len(vs) > 0 {
		t.Fatalf("recovery pin: %v", vs)
	}
	ctx := context.Background()
	if vs := probes.VerifyWitnesses(ctx, sm, 1, "test", true, tsPerturbNone); len(vs) > 0 {
		t.Fatalf("the witness read failed unperturbed:\n%v", vs)
	}
	if probes.Evidence().WitnessReadsAfterRecovery != 1 {
		t.Fatalf("WitnessReadsAfterRecovery = %d, want 1",
			probes.Evidence().WitnessReadsAfterRecovery)
	}

	sm2, probes2 := newTypedSchemaDurableFixture(t, typedSchemaDefaultSeed)
	if vs := probes2.RecoveryPin(sm2, 1, 1, tsPerturbNone); len(vs) > 0 {
		t.Fatalf("recovery pin: %v", vs)
	}
	vs := probes2.VerifyWitnesses(ctx, sm2, 1, "test", true, tsPerturbWitnessPoison)
	if len(vs) == 0 {
		t.Fatal("a witness carrying the REFUSED value satisfied the post-recovery clause: " +
			"the clause cannot fail")
	}
	if !tsViolationsMentionOp(vs, tsOp("witness:cypher")) && !tsViolationsMentionOp(vs, tsOp("witness:substrate")) {
		t.Fatalf("expected a witness clause to fire, got:\n%v", vs)
	}
}

// TestTypedSchema_EngineRejectionNeverReachesTheWAL is the arm-D1 claim in
// isolation, adjudicated on the durable image rather than inferred from the
// error: a refused Cypher write must leave nothing to replay.
//
// It is separate from the scenario because it asserts a DIFFERENCE — the accepted
// value came back and the refused one did not — and the scenario's witness clause
// asserts only the surviving value. An arm that checked absence alone would pass
// on a recovery that replayed nothing at all.
func TestTypedSchema_EngineRejectionNeverReachesTheWAL(t *testing.T) {
	sm, probes := newTypedSchemaDurableFixture(t, typedSchemaDefaultSeed)
	ctx := context.Background()
	if vs := probes.RecoveryPin(sm, 1, 1, tsPerturbNone); len(vs) > 0 {
		t.Fatalf("recovery pin: %v", vs)
	}
	w := probes.witnesses[0]
	got, isInt, err := tsAgeByName(ctx, sm.engine, w.name)
	if err != nil {
		t.Fatalf("projecting the witness age: %v", err)
	}
	if !isInt || got != w.acceptedAge {
		t.Fatalf("witness %q came back with integer=%t value=%d, want the ACCEPTED %d",
			w.name, isInt, got, w.acceptedAge)
	}
	sub := newTypedSchemaSubstrate(sm.graph())
	props, ok := sub.propsOf[w.name]
	if !ok {
		t.Fatalf("witness %q is absent from the native store after recovery", w.name)
	}
	stored := props[tsEngineKeyAge]
	if stored.Kind() != lpg.PropInt64 {
		t.Fatalf("witness %q carries %s after recovery: the REFUSED value reached the WAL",
			w.name, tsRender(stored, true))
	}
	t.Logf("witness %q: accepted INTEGER(%d) survived; the refused STRING(%q) never reached the log",
		w.name, w.acceptedAge, w.rejected)
}

// -----------------------------------------------------------------------------
// Arm D2 — the pure store/txn path, and the finding it pins
// -----------------------------------------------------------------------------

// TestTypedSchema_PureStoreArm drives the pure store/txn arm on its own and
// prints what it MEASURED, so the finding is legible from the log rather than
// only from a red run.
//
// What it pins (MEASURED 2026-08-24): `txn.Tx.Commit` appends and fsyncs every
// buffered op BEFORE it applies them, so a validator rejection during the apply
// returns txn.ErrCommittedNotApplied with the frame already durable — and the
// reopen, which installs no validator, materialises the value the live validator
// refused. The live graph is correctly left without it; the durable image is not.
//
// The Cypher engine cannot reach this ordering: walMutatorAdapter.SetNodeProperty
// performs the validated write before it buffers the WAL op, which is what
// TestTypedSchema_EngineRejectionNeverReachesTheWAL observes.
func TestTypedSchema_PureStoreArm(t *testing.T) {
	obs, err := typedSchemaPureStoreArm(NewSeed(typedSchemaDefaultSeed))
	if err != nil {
		t.Fatalf("pure-store arm: %v", err)
	}
	t.Logf("pure store/txn arm MEASURED: %s", obs.String())
	if vs := checkTypedSchemaPureStore(1, obs); len(vs) > 0 {
		t.Fatalf("the pure-store pin no longer holds:\n%v", vs)
	}
	if !obs.resurrected {
		t.Fatal("the refused value was NOT resurrected; checkTypedSchemaPureStore should have said so")
	}
	if obs.resurrectedKind != lpg.PropString {
		t.Fatalf("the resurrected value is %s, want the STRING that was committed",
			tsKindName(obs.resurrectedKind))
	}
}

// TestTypedSchema_PureStoreCheckIsFalsifiable proves every clause of
// [checkTypedSchemaPureStore] can fire, by handing it observations that violate
// each one. It is a table over the RECORD rather than over the store, because the
// record is exactly what the clauses read.
func TestTypedSchema_PureStoreCheckIsFalsifiable(t *testing.T) {
	t.Parallel()
	good := tsPureStoreObservation{
		acceptErr: "<nil>", rejectErr: "committed-not-applied", notApplied: true,
		typeMismatch: true, liveAbsent: true, acceptedSurvived: true,
		resurrected: true, resurrectedKind: lpg.PropString, stored: "STRING(\"x\")", walOps: 2,
	}
	if vs := checkTypedSchemaPureStore(1, good); len(vs) > 0 {
		t.Fatalf("the reference observation must pass:\n%v", vs)
	}
	cases := []struct {
		name   string
		mutate func(o *tsPureStoreObservation)
		clause string
	}{
		{"no ErrCommittedNotApplied", func(o *tsPureStoreObservation) { o.notApplied = false }, "pure-store:precondition"},
		{"wrong sentinel", func(o *tsPureStoreObservation) { o.typeMismatch = false }, "pure-store:precondition"},
		{"accepted commit failed", func(o *tsPureStoreObservation) { o.acceptErr = "boom" }, "pure-store:accepted-commit"},
		{"live graph mutated", func(o *tsPureStoreObservation) { o.liveAbsent = false }, "pure-store:live"},
		{"accepted value lost", func(o *tsPureStoreObservation) { o.acceptedSurvived = false }, "pure-store:accepted-survived"},
		{"no resurrection", func(o *tsPureStoreObservation) { o.resurrected = false }, "pure-store:resurrection-pin"},
		{"resurrected as another kind", func(o *tsPureStoreObservation) { o.resurrectedKind = lpg.PropInt64 }, "pure-store:resurrection-kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := good
			tc.mutate(&o)
			vs := checkTypedSchemaPureStore(1, o)
			if len(vs) == 0 {
				t.Fatalf("the %q clause cannot fail", tc.clause)
			}
			if !tsViolationsMentionOp(vs, tsOp(tc.clause)) {
				t.Fatalf("expected %q, got:\n%v", tc.clause, vs)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The model, adjudicated against the declaration table it is built from
// -----------------------------------------------------------------------------

// TestTypedSchemaModel_MatchesItsDeclarationTable pins the verdict model's own
// behaviour, so a change to the declaration table cannot silently change what
// the scenario expects.
//
// It deliberately does NOT compare the model with [schema.Schema]: that is the
// comparison the whole scenario performs, and duplicating it here would make this
// test pass whenever the two agree, including when both are wrong.
func TestTypedSchemaModel_MatchesItsDeclarationTable(t *testing.T) {
	t.Parallel()
	m := newTypedSchemaModel(typedSchemaDecls(), map[string][]string{tsSideLabel: {tsKeyStr}})

	writes := []struct {
		key   string
		value lpg.PropertyValue
		want  tsVerdict
	}{
		{tsKeyStr, lpg.StringValue("s"), tsAccept},
		{tsKeyStr, lpg.Int64Value(1), tsRejectTypeMismatch},
		{tsKeyInt, lpg.Int64Value(1), tsAccept},
		{tsKeyInt, lpg.Float64Value(1), tsRejectTypeMismatch},
		{tsKeyFloat, lpg.Float64Value(1), tsAccept},
		{tsKeyBool, lpg.BoolValue(true), tsAccept},
		{tsKeyBool, lpg.StringValue("true"), tsRejectTypeMismatch},
		{tsKeyGhost, lpg.StringValue("x"), tsRejectUnknownProperty},
	}
	for _, w := range writes {
		if got := m.predictWrite(w.key, w.value); got != w.want {
			t.Errorf("predictWrite(%q, %s) = %s, want %s", w.key, tsRender(w.value, true), got, w.want)
		}
	}

	nodes := []struct {
		name   string
		labels []string
		props  map[string]lpg.PropertyValue
		want   tsNodeVerdict
	}{
		{"unlabelled", nil, nil, tsNodeOK},
		{"labelled, required missing", []string{tsSideLabel}, nil, tsNodeMissingRequired},
		{"labelled, required present", []string{tsSideLabel},
			map[string]lpg.PropertyValue{tsKeyStr: lpg.StringValue("s")}, tsNodeOK},
		{"required present, another kind wrong", []string{tsSideLabel},
			map[string]lpg.PropertyValue{
				tsKeyStr: lpg.StringValue("s"), tsKeyInt: lpg.StringValue("wrong"),
			}, tsNodeTypeMismatch},
		{"unregistered key passes through", []string{tsSideLabel},
			map[string]lpg.PropertyValue{
				tsKeyStr: lpg.StringValue("s"), tsKeyGhost: lpg.BoolValue(true),
			}, tsNodeOK},
		// Required-existence is checked BEFORE the kind re-check, so a node that
		// violates both reports the existence failure. The order matters: the
		// scenario asserts WHICH sentinel is raised, not merely that one is.
		{"both violated reports existence", []string{tsSideLabel},
			map[string]lpg.PropertyValue{tsKeyInt: lpg.StringValue("wrong")}, tsNodeMissingRequired},
	}
	for _, n := range nodes {
		if got := m.predictNode(n.labels, n.props); got != n.want {
			t.Errorf("predictNode(%s) = %s, want %s", n.name, got, n.want)
		}
	}
}

// TestTypedSchema_SweepVisitsEveryCellExactlyOnce pins the sweep's structural
// claim, which every coverage gate rests on.
func TestTypedSchema_SweepVisitsEveryCellExactlyOnce(t *testing.T) {
	t.Parallel()
	seed := NewSeed(typedSchemaDefaultSeed)
	var seen [tsPathCount][tsVerdictCount]int
	cells := tsCells(seed)
	if want := int(tsPathCount) * int(tsVerdictCount); len(cells) != want {
		t.Fatalf("tsCells returned %d cells, want %d", len(cells), want)
	}
	for _, c := range cells {
		seen[c.path][c.want]++
	}
	for p := tsPath(0); p < tsPathCount; p++ {
		for v := tsVerdict(0); v < tsVerdictCount; v++ {
			if seen[p][v] != 1 {
				t.Errorf("cell (%s, %s) visited %d times in one epoch, want exactly 1", p, v, seen[p][v])
			}
		}
	}
}

// tsViolationsMentionOp reports whether any violation in vs carries the given op
// label.
//
// It matches on the OP rather than on the message, which the package's existing
// violationsMention does: these clauses are identified by their op label
// ([tsOp]), and matching a message substring would pass on any violation whose
// prose happened to contain the same words.
func tsViolationsMentionOp(vs []Violation, op string) bool {
	for _, v := range vs {
		if strings.Contains(v.Op, op) {
			return true
		}
	}
	return false
}
