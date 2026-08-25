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
//
// It also pins the ORDERING of the two fleet-measured instruments on real runs. A
// refusal drawn on a round trip that overlapped a honest exchange's own flight
// necessarily overlapped the window holding that flight, so the overlapping count
// can never exceed the window count. The gate depends on it: nv-swarm-overlap is
// gated on the window count being nonzero precisely so that one cause is never
// reported as two findings, and an instrument change that let overlap exceed the
// window would silently turn that gate into a filter that hides the harder clause.
// The evidence fixtures cannot pin it — they set both numbers by hand — so it is
// checked here, where the fleet produces them.
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
		if ev.RefusalsConcurrentWithHonest > ev.RefusalsSpanningHonestWindow {
			t.Errorf("seed %#x measured %d refusals overlapping honest FLIGHT but only %d overlapping "+
				"the honest WINDOW that contains it. The second must dominate the first by "+
				"construction, and nv-swarm-overlap is gated on it, so a run in this state would have "+
				"the gate suppress the harder clause:\n%s",
				seed, ev.RefusalsConcurrentWithHonest, ev.RefusalsSpanningHonestWindow, ev)
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

	// The pre-fleet boundary scan, recorded exactly as
	// [boltDecodeSwarm.boundaryScan] records it, and the leak probe sized to what
	// that scan MEASURED rather than to what the model predicts (rmp #2579).
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
	e.LeakProbeFinal = probe(e.MeasuredBoundary)

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
	// Every refusal was drawn on a round trip overlapping honest flight, which is
	// what a healthy concurrent run looks like: the honest client is in flight for
	// most of its own window, and an abusive round trip carries 4.5 MiB. A round
	// trip that overlapped honest flight necessarily overlapped the window holding
	// that flight, so the window count is the same number here.
	e.RefusalsConcurrentWithHonest = rejected
	e.RefusalsSpanningHonestWindow = rejected
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
			// The case that motivates the EXACTLY-four arity in
			// [boltDecodeClassification] (rmp #2575). The code's second segment still
			// reads "TransientError", so a guard that merely split on dots and took
			// parts[1] would certify this as retryable backpressure. The real driver
			// cannot classify it at all: neo4j-go-driver v5.28.4's
			// (*Neo4jError).parse abandons any code that is not four segments long
			// (neo4j/db/errors.go:121-123), leaving classification empty, so
			// IsRetriableTransient returns false and a client sees a hard failure
			// where the server meant "try again". The clause must therefore FIRE on
			// the shape, not be satisfied by the substring.
			name: "backpressure whose code the real driver cannot classify at all",
			perturb: func(e *BoltDecodeEvidence) {
				e.Arms[boltDecodeArmIndex(e, "pool-breach-write")].Code = "Neo.TransientError.OutOfMemoryError"
			},
			clause: "pool-refusal-retryable",
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

// -----------------------------------------------------------------------------
// The collapse-detection claim (rmp #2579)
// -----------------------------------------------------------------------------

// boltDecodeVectorOf classifies one arm into the abuse vector it represents,
// using EXACTLY the discrimination [checkBoltDecodeCapsAnswerDifferently] uses —
// the pool arm by name, then depth against the two caps, and only for a refused
// arm. Sharing the rule rather than restating it is what makes a collapse built
// here the same collapse that clause reads; it returns "" for an arm that is not
// one of the three refused vectors.
func boltDecodeVectorOf(a *BoltDecodeExchange) string {
	if a.Reply != "FAILURE" {
		return ""
	}
	switch {
	case a.Name == "pool-breach-write":
		return "aggregate pool"
	case a.Depth >= boltDecodeWireDepthCap:
		return "wire nesting cap"
	case a.Depth > boltDecodeParamDepthCap:
		return "engine parameter cap"
	}
	return ""
}

// boltDecodeObservedCodes returns the code each vector actually drew, by the same
// last-arm-wins rule the distinctness clause applies.
func boltDecodeObservedCodes(e *BoltDecodeEvidence) map[string]string {
	observed := map[string]string{}
	for i := range e.Arms {
		if vector := boltDecodeVectorOf(&e.Arms[i]); vector != "" {
			observed[vector] = e.Arms[i].Code
		}
	}
	return observed
}

// boltDecodeCollapseVector simulates a server that has stopped telling two abuse
// vectors apart, by rewriting EVERY arm of the from vector to answer the code the
// onto vector draws.
//
// Every arm, not the first: the nesting family carries several arms per vector
// (four for the engine's parameter cap, six for the wire cap) and the clause
// reads the LAST one, so perturbing a single arm would leave the observed code
// unchanged and the test would pass while measuring nothing.
func boltDecodeCollapseVector(e *BoltDecodeEvidence, from, onto string) {
	code := boltDecodeObservedCodes(e)[onto]
	for i := range e.Arms {
		if boltDecodeVectorOf(&e.Arms[i]) == from {
			e.Arms[i].Code = code
		}
	}
}

// boltDecodeLiteralPinningClauses are the clauses that compare an OBSERVED code
// against one of the scenario's own declared constants. They are the ones
// [checkBoltDecodeCapsAnswerDifferently]'s godoc names when it claims not to be
// the only thing standing between the run and a server that answers every abuse
// vector alike.
var boltDecodeLiteralPinningClauses = []string{
	"pool-refusal-typed",          // pins boltDecodeCodeBudget on the pool arm
	"pool-refusal-retryable",      // pins the classification segment of that code
	"nesting-answer",              // pins each nesting arm's exact expected code
	"nesting-is-not-backpressure", // pins boltDecodeCodeBudget off the nesting arms
	"nesting-message",             // pins the message literal that accompanies a code
}

// TestBoltDecodePressure_DistinctnessIsNeverTheSoleCollapseDetector pins a claim
// that until now rested on a measurement no longer in the tree (rmp #2579).
//
// rmp #2576 rewrote [checkBoltDecodeCapsAnswerDifferently]'s godoc to say that the
// clause is NOT the only thing that catches a server which has collapsed two
// abuse vectors onto one code, because the literal-pinning clauses catch such a
// collapse first. That wording was justified by a throwaway probe which drove all
// six pairwise collapse directions and recorded a literal-pinning clause
// co-firing in every one. The probe was then deleted, so the surviving claim
// rested on nothing executable: narrowing nesting-answer later would silently
// make the distinctness clause the sole detector and the godoc quietly false,
// and no test would object.
//
// This table is that probe, kept. It perturbs evidence structs and needs no
// server, so it costs nothing to run on every change.
//
// It asserts the claim in both halves, because either alone is satisfiable by
// accident: the distinctness clause must FIRE on the collapse (or it is not
// doing the job its name claims), and a literal-pinning clause must fire TOO (or
// the distinctness clause has become the sole detector, which is the state the
// godoc denies). The direction list is ENUMERATED from the vector order rather
// than written out, so a fourth abuse vector cannot be added without this table
// growing to cover it.
func TestBoltDecodePressure_DistinctnessIsNeverTheSoleCollapseDetector(t *testing.T) {
	t.Parallel()

	type direction struct{ from, onto string }
	var directions []direction
	for _, from := range boltDecodeVectorOrder {
		for _, onto := range boltDecodeVectorOrder {
			if from != onto {
				directions = append(directions, direction{from, onto})
			}
		}
	}
	// n*(n-1) ordered pairs over the three vectors the scenario drives.
	if want := len(boltDecodeVectorOrder) * (len(boltDecodeVectorOrder) - 1); len(directions) != want {
		t.Fatalf("enumerated %d collapse directions, want %d over %d vectors",
			len(directions), want, len(boltDecodeVectorOrder))
	}

	for _, d := range directions {
		t.Run("the "+d.from+" collapsed onto the "+d.onto, func(t *testing.T) {
			t.Parallel()
			ev := healthyBoltDecodeEvidence()

			before := boltDecodeObservedCodes(ev)
			if before[d.from] == before[d.onto] {
				t.Fatalf("the %s and the %s already answer %q in the HEALTHY fixture, so this "+
					"direction collapses nothing and the case is vacuous",
					d.from, d.onto, before[d.from])
			}
			boltDecodeCollapseVector(ev, d.from, d.onto)

			// The perturbation must have TAKEN. A collapse helper that missed an arm
			// would leave the observed code unchanged and every assertion below would
			// then be measuring the healthy fixture.
			after := boltDecodeObservedCodes(ev)
			if after[d.from] != after[d.onto] {
				t.Fatalf("the collapse did not take: the %s answers %q and the %s answers %q, "+
					"so no vector pair actually shares a code",
					d.from, after[d.from], d.onto, after[d.onto])
			}
			if len(after) != len(boltDecodeVectorOrder) {
				t.Fatalf("only %d of %d vectors are present after the collapse, so the distinctness "+
					"clause returns without adjudicating", len(after), len(boltDecodeVectorOrder))
			}

			v := checkBoltDecodePressure(ev)
			if !violationsMentionClause(v, "caps-answer-differently") {
				t.Fatalf("collapsing the %s onto the %s (both now answer %q) did not fire "+
					"caps-answer-differently, so the clause does not detect the very state it is "+
					"named for. Got:\n%s", d.from, d.onto, after[d.from], renderViolations(v))
			}

			var coFired []string
			for _, clause := range boltDecodeLiteralPinningClauses {
				if violationsMentionClause(v, clause) {
					coFired = append(coFired, clause)
				}
			}
			if len(coFired) == 0 {
				t.Fatalf("collapsing the %s onto the %s fired caps-answer-differently and NOTHING "+
					"that pins a literal, so that clause is now the SOLE detector of a collapsed "+
					"server. checkBoltDecodeCapsAnswerDifferently's godoc says it is not, and that "+
					"claim is now false: either restore the literal-pinning clause that was narrowed, "+
					"or rewrite the godoc to say the clause stands alone. Got:\n%s",
					d.from, d.onto, renderViolations(v))
			}
			t.Logf("collapse %s -> %s: caps-answer-differently fired, co-fired with %v",
				d.from, d.onto, coFired)
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
			// The fleet and the honest client took turns: the pressure was there for the
			// whole honest window, but every refusal was drawn on a round trip that
			// started and finished between two honest exchanges. Fires this clause alone
			// — RefusalsSpanningHonestWindow is untouched, so the density clause is
			// satisfied.
			name:    "the fleet and the honest client took turns instead of overlapping",
			perturb: func(e *BoltDecodeEvidence) { e.RefusalsConcurrentWithHonest = 0 },
			clause:  "nv-swarm-overlap",
		},
		{
			// Half one of the density clause: honest service ran entirely outside the
			// pressure window, so nothing it observed was under backpressure. Fires
			// this clause alone — nv-swarm-overlap is GATED on this same quantity, so
			// zeroing it silences overlap rather than firing it too.
			name:    "NO abusive round trip overlapping the honest window was refused",
			perturb: func(e *BoltDecodeEvidence) { e.RefusalsSpanningHonestWindow = 0 },
			clause:  "nv-swarm-pressure-density",
		},
		{
			// Half two: the start barrier expired, so honest service began before the
			// pressure had demonstrably built. Also fires this clause alone.
			name:    "the start barrier EXPIRED, so honest service did not begin inside the pressure window",
			perturb: func(e *BoltDecodeEvidence) { e.PressureStarted = false },
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
		{
			// The pre-fleet scan is what gives the leak probe a MEASURED size. If it
			// went entirely green the runner would silently fall back to the model's
			// boundary and nv-swarm-leak-probe-tight would go back to reading a
			// constant while still looking like a measurement (rmp #2579).
			name: "the pre-fleet scan never refused, so the leak probe has no MEASURED boundary to stand on",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Window {
					e.Window[i].Accepted, e.Window[i].Code = true, ""
				}
			},
			clause: "nv-swarm-window-spans-boundary",
		},
		{
			name: "the pre-fleet scan admitted nothing, so the pool did not start full",
			perturb: func(e *BoltDecodeEvidence) {
				for i := range e.Window {
					e.Window[i].Accepted, e.Window[i].Code = false, boltDecodeCodeBudget
				}
				e.MeasuredBoundary = -1
			},
			clause: "nv-swarm-window-spans-boundary",
		},
		{
			name:    "the scan ran but recorded no measured boundary, so the probe fell back to the model's",
			perturb: func(e *BoltDecodeEvidence) { e.MeasuredBoundary = -1 },
			clause:  "nv-swarm-window-spans-boundary",
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

// TestBoltDecodeSwarm_DensityAcceptsLoadedRuns pins the runs that made two
// successive versions of the density clause report a CLEAN engine as a failure
// (rmp #2587).
//
// Both shapes are transcribed from real evidence lines, not invented:
//
//   - `make ci`, 2026-08-20: 24 of 24 honest exchanges served correctly, all four
//     abuser connections alive, 18 abusive messages refused with the correct typed
//     error — and 7 refusals across the honest run, distributed [3 0 2 2]. The
//     first version of this clause failed it for 7 < 8; the second failed it for
//     the empty second segment.
//   - 32 concurrent -race test binaries: the same engine, 24 fleet messages, 2
//     refusals across the honest run, distributed [2 0 0 0].
//
// Neither shape may fire nv-swarm-pressure-density. The test asserts on that
// clause specifically rather than on the whole gate, so that a failure names the
// clause it is about. The deeper shape used to drop nv-swarm-overlap below its own
// floor as well — a second rate in the same gate, which this test deliberately did
// not absorb — and rmp #2596 has since removed that floor;
// [TestBoltDecodeSwarm_OverlapAcceptsLoadedRuns] pins the runs that exposed it.
func TestBoltDecodeSwarm_DensityAcceptsLoadedRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// perSegment is the refusal count to lay down inside each segment of the
		// honest run, reproducing a MEASURED distribution.
		perSegment []int64
	}{
		{name: "the make ci run of 2026-08-20", perSegment: []int64{3, 0, 2, 2}},
		{name: "32 concurrent -race test binaries", perSegment: []int64{2, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := healthyBoltDecodeSwarmEvidence()
			if len(tc.perSegment) != boltDecodeSwarmPressureSegments {
				t.Fatalf("fixture describes %d segments, want %d", len(tc.perSegment), boltDecodeSwarmPressureSegments)
			}

			// Lay the measured distribution over the honest run: every refusal in a
			// segment is attributed to that segment's FIRST exchange, which is enough
			// to reproduce the per-segment counts the real runs reported.
			var rejected int64
			n := len(e.Honest)
			for s, want := range tc.perSegment {
				lo := s * n / boltDecodeSwarmPressureSegments
				hi := (s + 1) * n / boltDecodeSwarmPressureSegments
				for i := lo; i < hi; i++ {
					e.Honest[i].RejectionsBefore = rejected
					if i == lo {
						rejected += want
					}
					e.Honest[i].RejectionsAfter = rejected
				}
			}
			e.RejectionsDuringHonest = rejected
			// Every refusal the counter recorded inside the window was drawn on a round
			// trip that ENDED inside the window, so it also overlapped the window. The
			// transcribed runs predate the instrument the clause now reads, and this is
			// the faithful floor their recorded numbers imply — never a larger number
			// invented to make the fixture pass.
			e.RefusalsSpanningHonestWindow = rejected
			e.PressureStarted = true

			// The fixture must really carry the measured shape, or it would pass for
			// the wrong reason.
			got := boltDecodeSwarmSegmentRefusals(e.Honest)
			for i := range tc.perSegment {
				if got[i] != tc.perSegment[i] {
					t.Fatalf("fixture built segments %v, want the measured %v", got, tc.perSegment)
				}
			}
			var total int64
			for _, x := range tc.perSegment {
				total += x
			}
			if e.RejectionsDuringHonest != total || total == 0 {
				t.Fatalf("fixture models %d refusals during honest service, want %d (nonzero)",
					e.RejectionsDuringHonest, total)
			}
			if e.RefusalsSpanningHonestWindow != total {
				t.Fatalf("fixture models %d refusals overlapping the honest window, want %d",
					e.RefusalsSpanningHonestWindow, total)
			}

			// The claim under test: this clause must NOT fire on any of these.
			for _, viol := range checkBoltDecodePressureNonVacuity(e) {
				if strings.Contains(viol.Op, "nv-swarm-pressure-density") {
					t.Errorf("nv-swarm-pressure-density fired on a measured CLEAN run "+
						"(segments %v, %d refusals during honest service, start barrier satisfied). "+
						"A density that machine load moves is not a coverage criterion:\n%s",
						got, e.RejectionsDuringHonest, renderViolations([]Violation{viol}))
				}
			}
		})
	}
}

// boltDecodeSwarmShape lays a MEASURED refusal placement over a healthy swarm
// fixture: which honest exchanges had refusals observed while they were in flight,
// and which gaps between exchanges had refusals observed inside them.
//
// The two maps are keyed by exchange index. inFlight[i] refusals are attributed to
// exchange i's own flight; gaps[i] refusals are attributed to the gap AFTER
// exchange i, which is where a real run puts everything the fleet drew while the
// honest client was between its own samples.
type boltDecodeSwarmShape struct {
	inFlight map[int]int64
	gaps     map[int]int64
	// concurrent is the run's measured count of refusals drawn on a round trip
	// overlapping honest flight — the quantity nv-swarm-overlap reads. It is
	// independent of where the counter samples fell, which is the whole point of
	// the instrument, so the fixture carries it as its own number. It can exceed
	// the total below, and in one transcribed run it does: a round trip that
	// overlapped honest flight can have its refusal counted after the last honest
	// exchange has closed.
	concurrent int64
	// tail is what the run counted between its last exchange's closing sample and
	// the deferred read of RejectionsDuringHonest. Real runs have one; carrying it
	// is what lets the fixture reproduce a measured (in flight, gaps, total) triple
	// exactly instead of one that happens to add up.
	tail int64
}

// apply rewrites the fixture's honest exchanges to carry the shape, and returns
// the fixture. The counter is walked forward exactly as a run walks it, so the
// evidence is coherent: every exchange's RejectionsBefore is the previous
// exchange's RejectionsAfter plus whatever landed in the gap between them.
func (sh boltDecodeSwarmShape) apply(e *BoltDecodeEvidence) *BoltDecodeEvidence {
	var counter int64
	for i := range e.Honest {
		if i > 0 {
			counter += sh.gaps[i-1]
		}
		e.Honest[i].RejectionsBefore = counter
		counter += sh.inFlight[i]
		e.Honest[i].RejectionsAfter = counter
	}
	e.RejectionsDuringHonest = counter - e.Honest[0].RejectionsBefore + sh.tail
	e.RefusalsConcurrentWithHonest = sh.concurrent
	// The window count is DERIVED, not transcribed, because every run below predates
	// the instrument that measures it. Both of the recorded numbers are lower bounds
	// on it — a refusal the counter recorded inside the window ended inside the
	// window, and a round trip overlapping honest FLIGHT overlaps the window holding
	// that flight — so their maximum is the faithful floor the recorded evidence
	// implies. Deriving it keeps the fixture from passing on a number chosen to make
	// it pass.
	e.RefusalsSpanningHonestWindow = e.RejectionsDuringHonest
	if sh.concurrent > e.RefusalsSpanningHonestWindow {
		e.RefusalsSpanningHonestWindow = sh.concurrent
	}
	e.PressureStarted = true
	return e
}

// TestBoltDecodeSwarm_OverlapAcceptsLoadedRuns pins the runs that made
// nv-swarm-overlap report a CLEAN engine as a failure (rmp #2596).
//
// Every shape is transcribed from real evidence, not invented. The clause used to
// require half of the four WIDE honest exchanges to have straddled a refusal, and
// each shape below is a run that missed that floor with one:
//
//   - `make ci`, 2026-08-21: the cover_gate run that opened the task reported
//     "only 1 of 4 WIDE honest exchanges STRADDLED a refusal, want at least 2 ...
//     1 narrow exchanges straddled". The engine was clean, and the same test passed
//     in the same invocation's race run. Its straddle counts are that line's; its
//     overlapping count is taken from a sweep run reporting the same two counts,
//     because the instrument that measures overlapping refusals post-dates the
//     failure and no run of that day recorded one.
//   - the rest come from a 96-run sweep of the swarm under 32 concurrent
//     coverage-instrumented test binaries on the reference host, in which 9 to 13
//     runs per sweep missed that floor and NOTHING else in the gate fired. They are
//     the run with the fewest overlapping refusals measured, the most gap-heavy one,
//     and one whose overlapping count EXCEEDS its window total — which happens when
//     a round trip that overlapped honest flight has its refusal counted after the
//     last honest exchange has closed, and is the clearest single sign that the two
//     instruments are measuring different things.
//
// The floor those runs missed is a rate: a wide exchange's window is a fixed 50 ms,
// while the interval between refusals is whatever the machine's load makes it. None
// of these shapes may fire any clause now that the question is whether any refusal
// was drawn on a round trip overlapping honest flight.
//
// This test is the regression gate on that: it fails against the thresholded clause
// and passes against the present one, verified by controlled reversion rather than
// assumed.
func TestBoltDecodeSwarm_OverlapAcceptsLoadedRuns(t *testing.T) {
	t.Parallel()
	// Wide exchanges are every boltDecodeSwarmWideEvery-th, so index 0 is wide and
	// indices 5, 7, 11 and 17 are narrow. Each shape puts refusals on exactly the
	// exchanges its measured straddle counts require.
	for _, tc := range []struct {
		name  string
		shape boltDecodeSwarmShape
		// The counts the real run reported, asserted against the fixture so that a
		// fixture drifting away from the measurement fails here rather than passing
		// for the wrong reason.
		wantWideStraddled   int
		wantNarrowStraddled int
		wantInFlight        int64
		wantGaps            int64
		wantDuring          int64
	}{
		{
			name: "the cover_gate run of 2026-08-21: 1 of 4 wide, 1 narrow",
			shape: boltDecodeSwarmShape{
				inFlight:   map[int]int64{0: 5, 7: 1},
				gaps:       map[int]int64{3: 4},
				concurrent: 10,
			},
			wantWideStraddled: 1, wantNarrowStraddled: 1,
			wantInFlight: 6, wantGaps: 4, wantDuring: 10,
		},
		{
			name: "the fewest overlapping refusals of 96 loaded runs: 4 of 6",
			shape: boltDecodeSwarmShape{
				inFlight:   map[int]int64{0: 2},
				gaps:       map[int]int64{3: 4},
				concurrent: 4,
			},
			wantWideStraddled: 1, wantNarrowStraddled: 0,
			wantInFlight: 2, wantGaps: 4, wantDuring: 6,
		},
		{
			name: "the most gap-heavy of 96 loaded runs: 14 refusals, 9 in the gaps",
			shape: boltDecodeSwarmShape{
				inFlight:   map[int]int64{0: 1, 5: 1, 11: 1, 17: 1},
				gaps:       map[int]int64{2: 5, 9: 4},
				concurrent: 11,
				tail:       1,
			},
			wantWideStraddled: 1, wantNarrowStraddled: 3,
			wantInFlight: 4, wantGaps: 9, wantDuring: 14,
		},
		{
			name: "a loaded run whose overlapping count EXCEEDS its window total: 9 of 6",
			shape: boltDecodeSwarmShape{
				inFlight:   map[int]int64{0: 4},
				gaps:       map[int]int64{3: 2},
				concurrent: 9,
			},
			wantWideStraddled: 1, wantNarrowStraddled: 0,
			wantInFlight: 4, wantGaps: 2, wantDuring: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := tc.shape.apply(healthyBoltDecodeSwarmEvidence())

			// The fixture must really carry the measured shape.
			wide, wideStraddled, narrowStraddled := 0, 0, 0
			for i := range e.Honest {
				straddled := e.Honest[i].RejectionsAfter > e.Honest[i].RejectionsBefore
				if e.Honest[i].Wide {
					wide++
					if straddled {
						wideStraddled++
					}
					continue
				}
				if straddled {
					narrowStraddled++
				}
			}
			inFlight := boltDecodeSwarmInFlightRefusals(e.Honest)
			gaps := boltDecodeSwarmGapRefusals(e.Honest)
			switch {
			case wide != boltDecodeSwarmMinWide:
				t.Fatalf("fixture ran %d wide exchanges, want %d", wide, boltDecodeSwarmMinWide)
			case wideStraddled != tc.wantWideStraddled || narrowStraddled != tc.wantNarrowStraddled:
				t.Fatalf("fixture built %d wide and %d narrow straddles, want the measured %d and %d",
					wideStraddled, narrowStraddled, tc.wantWideStraddled, tc.wantNarrowStraddled)
			case inFlight != tc.wantInFlight || gaps != tc.wantGaps:
				t.Fatalf("fixture built %d in-flight and %d gap refusals, want the measured %d and %d",
					inFlight, gaps, tc.wantInFlight, tc.wantGaps)
			case e.RejectionsDuringHonest != tc.wantDuring:
				t.Fatalf("fixture models %d refusals across the honest run, want the measured %d",
					e.RejectionsDuringHonest, tc.wantDuring)
			case e.RefusalsConcurrentWithHonest != tc.shape.concurrent || tc.shape.concurrent == 0:
				t.Fatalf("fixture models %d overlapping refusals, want the measured %d (nonzero)",
					e.RefusalsConcurrentWithHonest, tc.shape.concurrent)
			}

			// Every one of these shapes is a run the removed floor rejected.
			if wideStraddled >= boltDecodeSwarmMinWide/2 {
				t.Fatalf("fixture straddled %d of %d wide exchanges, which the removed floor of %d would "+
					"have ACCEPTED: this shape cannot regression-test the floor's removal",
					wideStraddled, wide, boltDecodeSwarmMinWide/2)
			}

			// The claim under test: no clause may fire on any of them.
			if v := checkBoltDecodePressureNonVacuity(e); len(v) > 0 {
				t.Errorf("the coverage gate fired on a MEASURED clean run (%d of %d wide exchanges "+
					"straddled, %d narrow; %d refusals in flight, %d in the gaps, %d overlapping a round "+
					"trip). A count of fixed 50 ms windows that each contained a refusal is a rate, and "+
					"machine load moves it:\n%s",
					wideStraddled, wide, narrowStraddled, inFlight, gaps,
					e.RefusalsConcurrentWithHonest, renderViolations(v))
			}
			if v := checkBoltDecodePressure(e); len(v) > 0 {
				t.Errorf("the contract fired on a MEASURED clean run:\n%s", renderViolations(v))
			}
		})
	}
}

// TestBoltDecodeSwarm_OverlapIsNotTheDensityClause proves the two clauses are not
// one predicate, in both directions.
//
// This matters because an existence claim is only worth having if it can fail on a
// shape a real run can produce. "The fleet and the honest client took turns" is
// that shape: the pressure was live for the whole honest window and none of it ever
// coincided with honest work in flight. It is what a server serialising honest
// statements against the fleet would produce, and the harness has been driven into
// it deliberately to check the instrument notices — the experiment is recorded on
// [boltDecodeSwarmInFlightRefusals], along with the earlier instrument that did NOT
// notice and is now reported rather than adjudicated.
//
// Density must stay silent on that shape and overlap must fire; a run with no
// pressure at all in the window is density's own subject and overlap must stay
// silent on it in turn, since it is gated on the window having held refusals at
// all. Either clause implying the other would make one of them incapable of being
// the clause that fires, and both firing together would name one cause twice.
func TestBoltDecodeSwarm_OverlapIsNotTheDensityClause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		shape  boltDecodeSwarmShape
		fires  string
		silent string
	}{
		{
			name: "the fleet and the honest client took turns: 12 refusals, none overlapping",
			// 12 refusals across the honest run, every one of them drawn on a round trip
			// that began and ended between two honest exchanges. Density counts all 12
			// and is satisfied; overlap counts none of them.
			shape:  boltDecodeSwarmShape{gaps: map[int]int64{2: 4, 9: 4, 16: 4}, concurrent: 0},
			fires:  "nv-swarm-overlap",
			silent: "nv-swarm-pressure-density",
		},
		{
			name: "no refusal landed anywhere in the honest window",
			// Nothing to attribute either way: the run was served outside the pressure
			// window altogether, which is the density clause's own subject.
			shape:  boltDecodeSwarmShape{},
			fires:  "nv-swarm-pressure-density",
			silent: "nv-swarm-overlap",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := tc.shape.apply(healthyBoltDecodeSwarmEvidence())
			v := checkBoltDecodePressureNonVacuity(e)
			if !violationsMentionClause(v, tc.fires) {
				t.Fatalf("shape %q did not fire %q, so that clause cannot fail on it. Got:\n%s",
					tc.name, tc.fires, renderViolations(v))
			}
			if tc.silent != "" && violationsMentionClause(v, tc.silent) {
				t.Errorf("shape %q fired %q as well as %q. The two clauses would then be one predicate, "+
					"and the gate would be reporting one finding twice while claiming two:\n%s",
					tc.name, tc.silent, tc.fires, renderViolations(v))
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
