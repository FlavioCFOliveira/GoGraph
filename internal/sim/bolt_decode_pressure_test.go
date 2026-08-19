package sim

// bolt_decode_pressure_test.go — the tests behind the Bolt inbound-decode
// pressure surface (rmp #2487).
//
// goleak.VerifyNone runs in every test that touches a live server: the swarm
// spawns five client goroutines and holds streams open across a 50 ms pause, which
// is exactly the shape that strands a reader. None of those tests call
// t.Parallel(), and that is not an oversight — goleak's default filters ignore a
// parent parked in testing.(*T).Run but NOT one parked in testing.tRunner.func1
// waiting for parallel subtests, so a parallel test that defers VerifyNone reports
// its own parent as a leak on every run. The pure-struct tests below take
// t.Parallel() and no goleak, which is the other half of the same rule.

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// boltDecodeTestSeeds are arbitrary but FIXED, so a failure is reproducible from
// the test log alone.
var boltDecodeTestSeeds = []uint64{0x2487_0001, 0x2487_00A7, 0x2487_0F0F}

// boltDecodeTestTimeout bounds one scenario run in a test.
const boltDecodeTestTimeout = 120 * time.Second

// -----------------------------------------------------------------------------
// Live runs
// -----------------------------------------------------------------------------

// TestBoltDecodePressure_Clean requires the deterministic surface to satisfy both
// the contract and the coverage gate at several seeds.
func TestBoltDecodePressure_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range boltDecodeTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), boltDecodeTestTimeout)
		ev, err := RunBoltDecodePressure(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if v := checkBoltDecodePressure(ev); len(v) > 0 {
			t.Errorf("seed %#x violates the contract:\n%s%s", seed, ev, renderViolations(v))
		}
		if v := checkBoltDecodePressureNonVacuity(ev); len(v) > 0 {
			t.Errorf("seed %#x fails the coverage gate:\n%s%s", seed, ev, renderViolations(v))
		}
	}
}

// TestBoltDecodeSwarm_Clean requires the concurrent surface to satisfy both
// adjudicators at several seeds.
func TestBoltDecodeSwarm_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range boltDecodeTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), boltDecodeTestTimeout)
		ev, err := RunBoltDecodeSwarm(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if v := checkBoltDecodePressure(ev); len(v) > 0 {
			t.Errorf("seed %#x violates the contract:\n%s%s", seed, ev, renderViolations(v))
		}
		if v := checkBoltDecodePressureNonVacuity(ev); len(v) > 0 {
			t.Errorf("seed %#x fails the coverage gate:\n%s%s", seed, ev, renderViolations(v))
		}
	}
}

// TestBoltDecodePressure_Determinism requires two runs of one seed to produce
// byte-identical evidence renderings.
//
// The rendering is the strong form of the claim: it covers the whole boundary
// window with its modelled holds, every arm's reply code and message, every
// nesting arm's depth and wire size, both leak probes and all three censuses at
// once. What it deliberately excludes is anything not reachable from the seed —
// node ids (a created node's hidden key comes from a process-global counter), the
// per-session id inside the engine's internal-error text (redacted before it is
// recorded), and the honest client's elapsed times. The SWARM is not covered
// here and cannot be: which abuser holds the pool on a given round is the
// scheduler's decision, which is exactly why it is registered as a separate
// concurrent scenario.
func TestBoltDecodePressure_Determinism(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seed = 0x2487_D371
	ctx, cancel := context.WithTimeout(context.Background(), 2*boltDecodeTestTimeout)
	defer cancel()

	first, err := RunBoltDecodePressure(ctx, seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltDecodePressure(ctx, seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if a, b := first.String(), second.String(); a != b {
		t.Fatalf("two runs of seed %#x rendered differently:\n--- first ---\n%s\n--- second ---\n%s", seed, a, b)
	}

	// The exclusion is PROVED, not trusted: the redacted internal-error text must
	// really have carried a per-session id, otherwise the redaction is dead code
	// hiding nothing and the rendering's stability says nothing about it.
	redacted := 0
	for i := range first.Arms {
		if strings.Contains(first.Arms[i].Message, "(session: <redacted>)") {
			redacted++
		}
	}
	if redacted == 0 {
		t.Errorf("no arm's message carried a redacted session id. The engine's parameter-cap refusal is " +
			"sanitised to \"An internal error occurred. See server logs for details (session: <16 hex>)\", " +
			"so if none is present the redaction is guarding nothing and the rendering's stability is " +
			"accidental rather than designed")
	}
}

// TestBoltDecodePressure_Scenario wires the catalogue: both scenarios must be
// registered and must pass at their default seeds.
func TestBoltDecodePressure_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	for _, name := range []string{ScenarioBoltDecodePressure, ScenarioBoltDecodeSwarm} {
		sc, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("scenario %q is not registered", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), boltDecodeTestTimeout)
		report, err := sc.Run(ctx, sc.DefaultSeed)
		cancel()
		if err != nil {
			t.Fatalf("scenario %q: %v", name, err)
		}
		if report != nil {
			t.Fatalf("scenario %q reported violations at its default seed: %s", name, report)
		}
	}
}

// TestBoltDecodePressure_Measurements logs the figures every oracle in this file
// rests on, so a run that quietly stopped exercising the surface is visible in the
// log rather than only in a threshold that has not yet been crossed.
func TestBoltDecodePressure_Measurements(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*boltDecodeTestTimeout)
	defer cancel()

	det, err := RunBoltDecodePressure(ctx, boltDecodePressureDefaultSeed)
	if err != nil {
		t.Fatalf("deterministic run: %v", err)
	}
	t.Logf("deterministic:\n%s", det)

	swarm, err := RunBoltDecodeSwarm(ctx, boltDecodeSwarmDefaultSeed)
	if err != nil {
		t.Fatalf("swarm run: %v", err)
	}
	t.Logf("swarm:\n%s", swarm)
}

// -----------------------------------------------------------------------------
// Hand-built evidence
// -----------------------------------------------------------------------------

// healthyBoltDecodeEvidence builds deterministic evidence that passes both
// adjudicators.
//
// It is generated from the package's own constants and from
// [boltDecodeNestArms] itself, never from literals: a change to a cap, to the
// window width or to the family's roster moves the fixture with the code, so it
// cannot stay healthy while a real run diverges from it. The expected nesting
// answers come from [boltDecodeExpectedNesting], the same function the checker
// uses — which is safe here precisely because the fixture is not the subject: its
// job is to be a baseline the perturbations move AWAY from, and every clause's
// falsifiability is proved by the perturbation firing, not by the baseline
// passing.
func healthyBoltDecodeEvidence() *BoltDecodeEvidence {
	e := &BoltDecodeEvidence{
		Seed:             0x2487_F00D,
		Budget:           boltDecodePressuredBudget,
		ControlBudget:    boltDecodeControlBudget,
		MeasuredBoundary: -1,
		LiveCensus:       map[string]int{boltDecodeFitLabel: 1, boltDecodeBreachLabel: 0},
		RecoveredCensus:  map[string]int{boltDecodeFitLabel: 1, boltDecodeBreachLabel: 0},
		ControlCensus:    map[string]int{boltDecodeBreachLabel: 1},
	}
	e.ModelBoundary = boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, e.Budget)

	probe := func(n int) BoltDecodeProbe {
		held := boltDecodeModelHeld(boltDecodeProbeQuery, boltDecodeParamKey, n)
		return BoltDecodeProbe{
			Elements: n, ModelHeld: held, Slack: e.Budget - held, Accepted: held <= e.Budget,
			Code: map[bool]string{true: "", false: boltDecodeCodeBudget}[held <= e.Budget],
		}
	}
	for d := -boltDecodeBoundaryWindow; d <= boltDecodeBoundaryWindow; d++ {
		p := probe(e.ModelBoundary + d)
		e.Window = append(e.Window, p)
		if p.Accepted && p.Elements > e.MeasuredBoundary {
			e.MeasuredBoundary = p.Elements
		}
	}
	e.LeakProbeInitial = probe(e.MeasuredBoundary)
	e.LeakProbeFinal = probe(e.MeasuredBoundary)

	// The two write arms and the control, sized from the model exactly as the run
	// sizes them.
	fitQuery := boltDecodeWriteQuery(boltDecodeFitLabel)
	fitN := boltDecodeModelElementsFor(fitQuery, boltDecodeParamKey, e.Budget) / 3
	e.Arms = append(e.Arms, BoltDecodeExchange{
		Name: "pool-fit-write", Vehicle: "run", Elements: fitN,
		ModelHeld:     boltDecodeModelHeld(fitQuery, boltDecodeParamKey, fitN),
		Reply:         "SUCCESS",
		SessionUsable: true, FollowUpValue: boltDecodeFollowUpValue("pool-fit-write"),
	})
	breachQuery := boltDecodeWriteQuery(boltDecodeBreachLabel)
	breachN := boltDecodeModelElementsFor(breachQuery, boltDecodeParamKey, e.Budget) * 2
	e.Arms = append(e.Arms,
		BoltDecodeExchange{
			Name: "pool-breach-write", Vehicle: "run", Elements: breachN,
			ModelHeld: boltDecodeModelHeld(breachQuery, boltDecodeParamKey, breachN),
			Reply:     "FAILURE", Code: boltDecodeCodeBudget, Message: boltDecodeMsgBudget,
			SessionUsable: true, FollowUpValue: boltDecodeFollowUpValue("pool-breach-write"),
		},
		BoltDecodeExchange{
			Name: "pool-control-raised-ceiling", Vehicle: "run", Elements: breachN,
			ModelHeld:     boltDecodeModelHeld(breachQuery, boltDecodeParamKey, breachN),
			Reply:         "SUCCESS",
			SessionUsable: true, FollowUpValue: boltDecodeFollowUpValue("pool-control-raised-ceiling"),
		})

	// The whole nesting family, at the depths the run drives and with the answers
	// the caps predict.
	const far = boltDecodeFarDepthMin
	for _, spec := range boltDecodeNestArms {
		depth := spec.depthOf(far)
		kind, code, usable := boltDecodeExpectedNesting(spec.vehicle, depth)
		arm := BoltDecodeExchange{
			Name: spec.name, Vehicle: spec.vehicle, Composite: spec.composite,
			Depth: depth, Elements: -1,
			WireBytes:     len(boltDecodeRawRun(boltDecodeProbeQuery, boltDecodeNestChain(depth, spec.composite))),
			Reply:         kind,
			Code:          code,
			SessionUsable: usable, FollowUpValue: boltDecodeFollowUpValue(spec.name),
		}
		switch code {
		case boltDecodeCodeInvalid:
			arm.Message = boltDecodeMsgInvalid
		case boltDecodeCodeInternal:
			arm.Message = boltDecodeMsgInternalPrefix + " See server logs for details (session: <redacted>)"
		}
		e.Arms = append(e.Arms, arm)
	}
	return e
}

// healthyBoltDecodeSwarmEvidence builds concurrent evidence that passes both
// adjudicators, with every count set to the smallest value that still clears its
// threshold plus a small margin — so a perturbation has to move it only a little
// to fire, and a threshold that has quietly drifted shows up here first.
func healthyBoltDecodeSwarmEvidence() *BoltDecodeEvidence {
	e := &BoltDecodeEvidence{
		Seed:             0x2487_BEEF,
		Swarm:            true,
		Budget:           boltDecodeSwarmBudget,
		Abusers:          boltDecodeSwarmAbusers,
		AbuserAccepted:   boltDecodeSwarmAbusers,
		AbuserRejected:   boltDecodeSwarmAbusers * boltDecodeSwarmMinRounds,
		AbuserReplies:    map[string]int{"SUCCESS": boltDecodeSwarmAbusers, boltDecodeCodeBudget: boltDecodeSwarmAbusers * boltDecodeSwarmMinRounds},
		AbuserAliveAfter: boltDecodeSwarmAbusers,
		PressureStarted:  true,
		LiveCensus:       map[string]int{},
		RecoveredCensus:  map[string]int{},
		ControlCensus:    map[string]int{},
		MeasuredBoundary: -1,
	}
	e.ModelBoundary = boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, e.Budget)
	held := boltDecodeModelHeld(boltDecodeProbeQuery, boltDecodeParamKey, e.ModelBoundary)
	e.LeakProbeFinal = BoltDecodeProbe{
		Elements: e.ModelBoundary, ModelHeld: held, Slack: e.Budget - held, Accepted: true,
	}

	var rejected int64
	for i := range boltDecodeSwarmHonestOps {
		h := BoltDecodeHonest{
			Index: i, Wide: i%boltDecodeSwarmWideEvery == 0,
			RejectionsBefore: rejected,
			Value:            int64(i), OK: true, Elapsed: time.Millisecond,
		}
		// Every wide exchange straddles; the narrow ones do not, which is the shape a
		// real run produces and keeps the fixture from making the clause pass for the
		// wrong reason.
		if h.Wide {
			rejected += 3
		}
		h.RejectionsAfter = rejected
		e.Honest = append(e.Honest, h)
	}
	e.RejectionsDuringHonest = rejected
	return e
}

// TestBoltDecodePressure_HealthyEvidencePasses is the precondition for both
// falsifiability tables: a perturbation that fires a clause proves nothing unless
// the unperturbed fixture passes every clause first.
func TestBoltDecodePressure_HealthyEvidencePasses(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]*BoltDecodeEvidence{
		"deterministic": healthyBoltDecodeEvidence(),
		"swarm":         healthyBoltDecodeSwarmEvidence(),
	} {
		if v := checkBoltDecodePressure(ev); len(v) > 0 {
			t.Errorf("the hand-built healthy %s evidence fails the contract:\n%s", name, renderViolations(v))
		}
		if v := checkBoltDecodePressureNonVacuity(ev); len(v) > 0 {
			t.Errorf("the hand-built healthy %s evidence fails the coverage gate:\n%s", name, renderViolations(v))
		}
	}
}

// boltDecodeArmIndex returns the index of the named arm. It PANICS when the arm
// is absent rather than calling t.Fatalf: every caller is a mutate closure running
// inside a t.Run subtest, and testing requires Fatalf to be called on the
// goroutine of the test it belongs to. A missing arm is a defect in the fixture,
// not a test outcome, so a panic is the honest signal and it names the arm.
func boltDecodeArmIndex(e *BoltDecodeEvidence, name string) int {
	for i := range e.Arms {
		if e.Arms[i].Name == name {
			return i
		}
	}
	panic("sim: test fixture has no arm named " + name)
}

// boltDecodeNestArmAt returns the first nesting arm whose expected code is want.
func boltDecodeNestArmAt(e *BoltDecodeEvidence, want string) int {
	for i := range e.Arms {
		if e.Arms[i].Depth > 0 && e.Arms[i].Code == want {
			return i
		}
	}
	panic("sim: test fixture has no nesting arm answering " + want)
}

// TestBoltDecodePressure_ContractCanFail proves every contract clause can fail.
// Each case's name is written as the DEFECT it represents, not as the field it
// touches, so a reader learns what the clause defends.
func TestBoltDecodePressure_ContractCanFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perturb func(*BoltDecodeEvidence)
		clause  string
	}{
		{
			name: "the pool served a message its own arithmetic cannot fit",
			perturb: func(e *BoltDecodeEvidence) {
				last := len(e.Window) - 1
				e.Window[last].Accepted, e.Window[last].Code = true, ""
			},
			clause: "pool-model-agreement",
		},
		{
			name: "admission is not monotone in the charge, so the boundary is not arithmetic",
			perturb: func(e *BoltDecodeEvidence) {
				e.Window[0].Accepted, e.Window[0].Code = false, boltDecodeCodeBudget
			},
			clause: "pool-boundary-monotone",
		},
		{
			name:    "a packstream per-slot cost changed and the boundary moved with it",
			perturb: func(e *BoltDecodeEvidence) { e.ModelBoundary-- },
			clause:  "pool-boundary-matches-model",
		},
		{
			name: "the breach arm was SERVED, so the pool never refused the message the run is built on",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Reply = "SUCCESS"
			},
			clause: "pool-breach-refused",
		},
		{
			name: "backpressure answered with a code no driver will retry",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Code = boltDecodeCodeInvalid
			},
			clause: "pool-refusal-retryable",
		},
		{
			name: "backpressure answered with a retryable classification but the wrong code",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Code = "Neo.TransientError.General.DatabaseUnavailable"
			},
			clause: "pool-refusal-typed",
		},
		{
			name: "the refusal no longer says WHICH ceiling was reached",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Message = "something went wrong"
			},
			clause: "pool-refusal-message",
		},
		{
			name: "backpressure cost the client its session",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].SessionUsable = false
			},
			clause: "pool-refusal-keeps-session",
		},
		{
			name: "a message the pool ACCEPTED was not served",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-fit-write")].Reply = "FAILURE"
			},
			clause: "pool-fit-served",
		},
		{
			name:    "the accepted write did not survive recovery",
			perturb: func(e *BoltDecodeEvidence) { e.RecoveredCensus[boltDecodeFitLabel] = 0 },
			clause:  "pool-fit-durable",
		},
		{
			name:    "a REFUSED write left its node behind, so the refusal was not atomic",
			perturb: func(e *BoltDecodeEvidence) { e.LiveCensus[boltDecodeBreachLabel] = 1 },
			clause:  "pool-breach-no-effect",
		},
		{
			name:    "the refused write survived a crash, so it had reached the WAL",
			perturb: func(e *BoltDecodeEvidence) { e.RecoveredCensus[boltDecodeBreachLabel] = 1 },
			clause:  "pool-breach-no-effect",
		},
		{
			name: "raising the ceiling changed nothing, so the pool is not what refused",
			perturb: func(e *BoltDecodeEvidence) {
				i := boltDecodeArmIndex(e, "pool-control-raised-ceiling")
				e.Arms[i].Reply, e.Arms[i].Code = "FAILURE", boltDecodeCodeBudget
			},
			clause: "pool-control-served",
		},
		{
			name: "the control replayed DIFFERENT bytes, so it controls for nothing",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-control-raised-ceiling")].Elements++
			},
			clause: "pool-control-identical-payload",
		},
		{
			name:    "the control served the message but wrote nothing",
			perturb: func(e *BoltDecodeEvidence) { e.ControlCensus[boltDecodeBreachLabel] = 0 },
			clause:  "pool-control-effect",
		},
		{
			name: "the wire nesting cap stopped refusing at its documented depth",
			perturb: func(e *BoltDecodeEvidence) {
				i := boltDecodeNestArmAt(e, boltDecodeCodeInvalid)
				e.Arms[i].Reply, e.Arms[i].Code, e.Arms[i].Message = "SUCCESS", "", ""
			},
			clause: "nesting-answer",
		},
		{
			name: "a stack-overflow attempt was answered as memory backpressure, telling the driver to RETRY it",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeNestArmAt(e, boltDecodeCodeInvalid)].Code = boltDecodeCodeBudget
			},
			clause: "nesting-is-not-backpressure",
		},
		{
			name: "a decode-layer refusal left the session FAILED where it should stay READY",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeNestArmAt(e, boltDecodeCodeInvalid)].SessionUsable = false
			},
			clause: "nesting-session-after",
		},
		{
			name: "the deep payload grew big enough to have been refused for its SIZE",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeNestArmAt(e, boltDecodeCodeInvalid)].WireBytes = boltDecodeNestingWireCeiling
			},
			clause: "nesting-not-by-size",
		},
		{
			name: "the wire-cap refusal's message changed under the same code",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeNestArmAt(e, boltDecodeCodeInvalid)].Message = "nope"
			},
			clause: "nesting-message",
		},
		{
			name: "the engine's parameter cap collapsed onto the wire cap's code",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Arms {
					if e.Arms[i].Code == boltDecodeCodeInternal {
						e.Arms[i].Code, e.Arms[i].Message = boltDecodeCodeInvalid, boltDecodeMsgInvalid
					}
				}
			},
			clause: "caps-answer-differently",
		},
		{
			name: "the pool did not return what it lent",
			perturb: func(e *BoltDecodeEvidence) {
				e.LeakProbeFinal.Accepted, e.LeakProbeFinal.Code = false, boltDecodeCodeBudget
			},
			clause: "budget-restored",
		},
		{
			name:    "the calibration probe was refused against a PRISTINE pool, so the leak probe measures nothing",
			perturb: func(e *BoltDecodeEvidence) { e.LeakProbeInitial.Accepted = false },
			clause:  "budget-probe-calibrated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := healthyBoltDecodeEvidence()
			tc.perturb(ev)
			v := checkBoltDecodePressure(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q, so that clause cannot fail. Got:\n%s",
					tc.name, tc.clause, renderViolations(v))
			}
		})
	}
}

// TestBoltDecodePressure_NonVacuityCanFail is the falsifiability proof for the
// GATE. It is a separate table from the contract's because the two adjudicators
// answer different questions — did the server misbehave, and was the run in a
// position to notice — and a gate clause that cannot fire is a gate that
// certifies nothing.
func TestBoltDecodePressure_NonVacuityCanFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perturb func(*BoltDecodeEvidence)
		clause  string
	}{
		{
			name: "the window never bracketed the boundary, so monotonicity had nothing to be monotone about",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Window {
					e.Window[i].Accepted, e.Window[i].Code = true, ""
				}
			},
			clause: "nv-window-spans-boundary",
		},
		{
			name: "the aggregate ceiling never fired, so every clause about how it fires passed untested",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Window {
					e.Window[i].Accepted, e.Window[i].Code = true, ""
				}
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Reply = "SUCCESS"
			},
			clause: "nv-pool-refusals-observed",
		},
		{
			name: "the pool admitted nothing, so 'every refusal is typed' is satisfied by a broken pool",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Window {
					e.Window[i].Accepted, e.Window[i].Code = false, boltDecodeCodeBudget
				}
				for i := range e.Arms {
					if e.Arms[i].Elements >= 0 {
						e.Arms[i].Reply, e.Arms[i].Code = "FAILURE", boltDecodeCodeBudget
					}
				}
			},
			clause: "nv-pool-accepts-observed",
		},
		{
			name:    "the leak probe went slack, so a pool that came back short would still admit it",
			perturb: func(e *BoltDecodeEvidence) { e.LeakProbeFinal.Slack = boltDecodeLeakSensitivity },
			clause:  "nv-leak-probe-tight",
		},
		{
			name:    "the leak probe does not fit even a pristine pool, so its refusal says nothing about a leak",
			perturb: func(e *BoltDecodeEvidence) { e.LeakProbeFinal.Slack = -1 },
			clause:  "nv-leak-probe-tight",
		},
		{
			name: "a nesting arm silently stopped running",
			perturb: func(e *BoltDecodeEvidence) {
				i := boltDecodeArmIndex(e, boltDecodeNestArms[0].name)
				e.Arms = append(e.Arms[:i], e.Arms[i+1:]...)
			},
			clause: "nv-nesting-family-complete",
		},
		{
			name: "the family stopped producing three distinct answers, so the distinctness clause returns silently",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Arms {
					if e.Arms[i].Code == boltDecodeCodeInternal {
						e.Arms[i].Reply, e.Arms[i].Code, e.Arms[i].Message = "SUCCESS", "", ""
					}
				}
			},
			clause: "nv-nesting-three-outcomes",
		},
		{
			name: "no over-nested HELLO was refused, so the PRE-AUTHENTICATION path was never visited",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Arms {
					if e.Arms[i].Vehicle == "hello" {
						e.Arms[i].Reply, e.Arms[i].Code, e.Arms[i].Message = "SUCCESS", "", ""
					}
				}
			},
			clause: "nv-preauth-refusal-observed",
		},
		{
			name:    "the control has the same ceiling, so the two servers are not an A/B on the ceiling at all",
			perturb: func(e *BoltDecodeEvidence) { e.ControlBudget = e.Budget },
			clause:  "nv-control-differs",
		},
		{
			name: "the control and the subject agreed, so the A/B did not separate",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-control-raised-ceiling")].Reply = "FAILURE"
			},
			clause: "nv-control-differs",
		},
		{
			name:    "nothing was written, so 'the refused write left nothing behind' is trivially true",
			perturb: func(e *BoltDecodeEvidence) { e.LiveCensus[boltDecodeFitLabel] = 0 },
			clause:  "nv-census-nonempty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := healthyBoltDecodeEvidence()
			tc.perturb(ev)
			v := checkBoltDecodePressureNonVacuity(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire gate clause %q, so that clause cannot fail. Got:\n%s",
					tc.name, tc.clause, renderViolations(v))
			}
		})
	}
}

// TestBoltDecodeSwarm_ContractCanFail proves every concurrent contract clause can
// fail.
func TestBoltDecodeSwarm_ContractCanFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perturb func(*BoltDecodeEvidence)
		clause  string
	}{
		{
			name: "an abusive message drew a third code, so pressure is not answered uniformly",
			perturb: func(e *BoltDecodeEvidence) {
				e.AbuserReplies["Neo.ClientError.Request.Invalid"] = 1
			},
			clause: "swarm-replies-typed",
		},
		{
			name: "the refusals stopped being retryable, so a driver treats backpressure as a hard failure",
			perturb: func(e *BoltDecodeEvidence) {
				n := e.AbuserReplies[boltDecodeCodeBudget]
				delete(e.AbuserReplies, boltDecodeCodeBudget)
				e.AbuserReplies["Neo.DatabaseError.General.OutOfMemoryError"] = n
			},
			clause: "swarm-refusal-retryable",
		},
		{
			name: "an abuser lost its CONNECTION instead of drawing a typed refusal",
			perturb: func(e *BoltDecodeEvidence) {
				e.TransportErrors = append(e.TransportErrors, "abuser 2 round 3: EOF")
			},
			clause: "swarm-no-transport-loss",
		},
		{
			name:    "an honest exchange came back with the WRONG row while the fleet was refused",
			perturb: func(e *BoltDecodeEvidence) { e.Honest[1].Value = 9999 },
			clause:  "swarm-honest-served",
		},
		{
			name:    "an honest exchange was STARVED under aggregate pressure",
			perturb: func(e *BoltDecodeEvidence) { e.Honest[1].Elapsed = boltDecodeHonestBound + time.Second },
			clause:  "swarm-honest-live",
		},
		{
			name:    "a refused abuser lost its connection by the end of the run",
			perturb: func(e *BoltDecodeEvidence) { e.AbuserAliveAfter-- },
			clause:  "swarm-abusers-alive",
		},
		{
			name: "the shared pool did not come back after the swarm quiesced",
			perturb: func(e *BoltDecodeEvidence) {
				e.LeakProbeFinal.Accepted, e.LeakProbeFinal.Code = false, boltDecodeCodeBudget
			},
			clause: "swarm-budget-restored",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := healthyBoltDecodeSwarmEvidence()
			tc.perturb(ev)
			v := checkBoltDecodePressure(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q, so that clause cannot fail. Got:\n%s",
					tc.name, tc.clause, renderViolations(v))
			}
		})
	}
}

// TestBoltDecodeSwarm_NonVacuityCanFail proves every concurrent gate clause can
// fail.
func TestBoltDecodeSwarm_NonVacuityCanFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perturb func(*BoltDecodeEvidence)
		clause  string
	}{
		{
			name:    "the pool was never pressured, so every clause about how it refuses passed untested",
			perturb: func(e *BoltDecodeEvidence) { e.AbuserRejected = 0 },
			clause:  "nv-swarm-rejections",
		},
		{
			name:    "everything was refused, so the refusals show a BROKEN pool rather than a FULL one",
			perturb: func(e *BoltDecodeEvidence) { e.AbuserAccepted = 0 },
			clause:  "nv-swarm-accepts",
		},
		{
			name: "honest service did not overlap the pressure: it ran BEFORE or AFTER it, never DURING",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Honest {
					e.Honest[i].RejectionsAfter = e.Honest[i].RejectionsBefore
				}
			},
			clause: "nv-swarm-overlap",
		},
		{
			name:    "the pressure was too thin across the honest run for the overlap to mean anything",
			perturb: func(e *BoltDecodeEvidence) { e.RejectionsDuringHonest = 1 },
			clause:  "nv-swarm-pressure-density",
		},
		{
			name:    "the honest client stopped early, so it sampled less of the pressure window than intended",
			perturb: func(e *BoltDecodeEvidence) { e.Honest = e.Honest[:2] },
			clause:  "nv-swarm-honest-count",
		},
		{
			name: "no WIDE exchange ran, so the overlap clause gates on nothing",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Honest {
					e.Honest[i].Wide = false
				}
			},
			clause: "nv-swarm-wide-exchanges",
		},
		{
			name:    "the leak probe went slack, so a pool that came back short would still admit it",
			perturb: func(e *BoltDecodeEvidence) { e.LeakProbeFinal.Slack = boltDecodeLeakSensitivity },
			clause:  "nv-swarm-leak-probe-tight",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := healthyBoltDecodeSwarmEvidence()
			tc.perturb(ev)
			v := checkBoltDecodePressureNonVacuity(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire gate clause %q, so that clause cannot fail. Got:\n%s",
					tc.name, tc.clause, renderViolations(v))
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The seed-mix guard
// -----------------------------------------------------------------------------

// boltSeedMixPairs is every (catalogue default seed, seed mix) pair the Bolt
// wire-surface scenarios XOR together, with the file:line of the XOR site so a
// failure points straight at the code rather than at this table.
//
// It is a TABLE and not a walk of [DefaultRegistry] because the registry cannot
// supply it: [Scenario] carries Name, DefaultSeed and Mode but no seed mix, and
// the mixes are unexported constants private to each scenario's file. A table is
// the closest thing to iteration that the types allow, and it is still a strict
// improvement on what it generalises — two single-scenario copies of the same
// three-line check, one per surface, which is why the collision this table found
// had gone unnoticed. (Those two copies still stand; they belong to other tasks
// and are now subsumed by this one.)
var boltSeedMixPairs = []struct {
	scenario string
	site     string
	seed     uint64
	mix      uint64
	// noMix records a scenario that XORs nothing at all, verified by reading its
	// runner rather than assumed from the absence of a constant. Such a scenario
	// has no mix to cancel its default seed, so it is listed and skipped instead of
	// being given a mix of 0 — which would be a false entry that passes the check
	// for the wrong reason.
	noMix bool
}{
	{scenario: ScenarioBoltAuth, site: "bolt_auth_surface.go", seed: boltAuthDefaultSeed, mix: boltAuthSeedMix},
	{scenario: ScenarioBoltCertRotation, site: "bolt_cert_rotation.go", seed: certRotationDefaultSeed, mix: certRotationSeedMix},
	{scenario: ScenarioBoltCertRotation, site: "bolt_cert_rotation.go (disk)", seed: certRotationDefaultSeed, mix: certRotationDiskSeedMix},
	{scenario: ScenarioBoltTxRegistry, site: "bolt_tx_registry.go", seed: boltTxRegistryDefaultSeed, mix: txAbandonSeedMix},
	{scenario: ScenarioBoltTxQuota, site: "bolt_tx_quota.go:415", seed: boltTxQuotaDefaultSeed, mix: txQuotaSeedMix},
	{scenario: ScenarioBoltShutdownDrain, site: "bolt_shutdown_drain.go", seed: boltShutdownDrainDefaultSeed, mix: boltDrainDiskSeedMix},
	{scenario: ScenarioBoltShutdownFleet, site: "bolt_shutdown_drain.go", seed: boltShutdownFleetDefaultSeed, mix: boltDrainDiskSeedMix},
	{scenario: ScenarioBoltStreaming, site: "bolt_stream_semantics.go:447", seed: boltStreamDefaultSeed, mix: boltStreamSeedMix},
	// RunBoltStreamStall builds no SimDisk and sub-seeds nothing: it calls
	// NewSeed(seed) directly (bolt_stream_semantics.go:1256) and the draw feeds only
	// the stall duration.
	{scenario: ScenarioBoltStreamingStall, site: "bolt_stream_semantics.go:1256", seed: boltStreamStallDefaultSeed, noMix: true},
	{scenario: ScenarioBoltBeginExtras, site: "bolt_begin_extras.go:593", seed: boltBeginExtrasDefaultSeed, mix: beginSeedMix},
	{scenario: ScenarioBoltVersionMatrix, site: "bolt_version_matrix.go", seed: boltVersionMatrixDefaultSeed, mix: boltVersionSeedMix},
	{scenario: ScenarioBoltDecodePressure, site: "bolt_decode_pressure.go", seed: boltDecodePressureDefaultSeed, mix: boltDecodeSeedMix},
	{scenario: ScenarioBoltDecodeSwarm, site: "bolt_decode_pressure.go", seed: boltDecodeSwarmDefaultSeed, mix: boltDecodeSwarmSeedMix},
}

// TestBoltScenarios_SeedMixDoesNotCancelTheDefaultSeed guards every Bolt
// scenario's decorrelating mix against its own catalogue default.
//
// XOR is self-annihilating: a mix equal to the default seed makes the ONE run
// every report starts from draw from NewSeed(0), so the mix decorrelates nothing
// on precisely the run that matters most and the sub-seeded component runs on a
// degenerate stream. rmp #2485 shipped 0x2485_B0_0C against a default of
// 0x2485_B00C, which Go reads as the same number because digit separators are
// cosmetic, and wrote a guard for it — but as a copy per surface, so nothing
// checked the surfaces that already existed.
//
// Generalising it immediately found one: bolt-tx-quota's mix was byte-identical
// to its default seed.
func TestBoltScenarios_SeedMixDoesNotCancelTheDefaultSeed(t *testing.T) {
	t.Parallel()
	for _, p := range boltSeedMixPairs {
		if p.noMix || p.seed^p.mix != 0 {
			continue
		}
		t.Errorf("%s: the seed mix at %s (%#x) equals the catalogue default seed (%#x), so the default "+
			"run draws from NewSeed(0) and the mix decorrelates nothing on the one run every report "+
			"starts from; pick a mix that differs from the default seed",
			p.scenario, p.site, p.mix, p.seed)
	}
}

// TestBoltScenarios_SeedMixTableCoversEveryBoltScenario keeps the table above
// from silently going stale: every Bolt scenario in the catalogue must appear in
// it, so adding a scenario without adding its mix fails here rather than leaving
// the new surface unguarded.
//
// A scenario with genuinely no seed mix has none to check, so the table is the
// only place that can record that fact; listing it with mix == 0 would be a lie.
// One Bolt scenario is in that position today — bolt-streaming-stall, which calls
// NewSeed(seed) directly and sub-seeds nothing — and it is carried with noMix set
// and its reason written down. Any further one takes the same honest route rather
// than a widened exemption.
func TestBoltScenarios_SeedMixTableCoversEveryBoltScenario(t *testing.T) {
	t.Parallel()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	covered := map[string]bool{}
	for _, p := range boltSeedMixPairs {
		covered[p.scenario] = true
	}
	for _, name := range reg.Names() {
		if !strings.HasPrefix(name, "bolt-") || covered[name] {
			continue
		}
		t.Errorf("catalogue scenario %q is a Bolt surface but has no entry in boltSeedMixPairs, so its "+
			"seed mix is unguarded against cancelling its own default seed", name)
	}
}
