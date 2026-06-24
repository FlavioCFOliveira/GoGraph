package sim

import (
	"context"
	"fmt"
)

// constraintEnforceDDL declares the UNIQUE (Person, name) constraint the
// constraint-enforcement scenario verifies. The engine parses the ON/ASSERT
// form (cypher/ir/ddl_parser.go); the constraint is appended to the WAL and
// fsynced, so it survives crash/recovery and is re-registered on reopen.
const constraintEnforceDDL = "CREATE CONSTRAINT sim_person_name_unique ON (n:Person) ASSERT n.name IS UNIQUE"

// constraintEnforceScenario verifies UNIQUE constraint enforcement under the
// DST: it creates a UNIQUE (Person, name) constraint, then drives a workload that
// interleaves fresh-name CREATEs (which must commit) with duplicate-name CREATEs
// (which the engine MUST reject with a typed constraint-violation error, applying
// nothing). The oracle predicts each outcome; a disagreement between the engine
// and the oracle is an enforcement gap. Deterministic crash+recovery cycles then
// prove the constraint survives recovery still enforcing. It is bit-reproducible.
func constraintEnforceScenario() Scenario {
	return Scenario{
		Name:        ScenarioConstraintEnforce,
		Description: "UNIQUE(Person.name) enforcement: duplicate CREATEs rejected, constraint survives crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xC047A157,
		MaxTicks:    500,
		// A durable store is required so the constraint (a WAL-logged schema
		// change) survives the crash cycles; crashes are moderate so several
		// recovery boundaries are exercised within the budget.
		Crash: CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:   runConstraintEnforce,
	}
}

// runConstraintEnforce is the constraint-enforcement custom run. It drives a
// deterministic constraint-aware loop directly (rather than the generic safety
// loop) so it can compare, per write, the engine's accept/reject outcome against
// the oracle's prediction — the heart of constraint verification. Crashes reuse
// [Simulator.maybeCrash] (drop the engine, reopen via real recovery, durability
// check); after each the constraint must still be enforced.
func runConstraintEnforce(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := constraintEnforceScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	// Declare the UNIQUE constraint in the engine and model it in the oracle.
	if err := sm.engineRunDDL(ctx, constraintEnforceDDL); err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce create constraint: %w", err)
	}
	sm.Oracle().SetUniqueOnName(true)

	report, err := sm.runConstraintLoop(ctx)
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce run: %w", err)
	}
	return report, nil
}

// runConstraintLoop drives the constraint-enforcement safety loop: each tick it
// emits a CREATE that is either a fresh unique name (must commit) or a duplicate
// of an existing name (must be rejected by the UNIQUE constraint), runs it
// against the engine, and asserts the engine's accept/reject outcome matches the
// oracle's prediction. A mismatch — the engine accepting a duplicate the
// constraint should forbid, or rejecting a valid create — is an
// ACID_CONSISTENCY violation (the engine failed to enforce a declared invariant).
// Crashes reuse [Simulator.maybeCrash]; the post-recovery enforcement is verified
// by the same per-op comparison continuing against the recovered engine.
func (s *Simulator) runConstraintLoop(ctx context.Context) (*SimReport, error) {
	freshCounter := 0
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		if report, err := s.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}

		op := s.nextConstraintOp(&freshCounter)
		engineCommitted := s.execute(ctx, op)
		predicted := s.oracle.ApplyCreate(op.Cypher, op.Params)
		if !engineCommitted {
			// A rejected write under this scenario is a UNIQUE-violating duplicate
			// the constraint forbade; count it as the non-vacuity guard.
			s.rejectedWrites++
		}

		if engineCommitted != predicted.Committed {
			v := Violation{
				Kind: ViolationACIDConsistency,
				Tick: tick,
				Op:   "constraint enforcement",
				Message: fmt.Sprintf(
					"UNIQUE(Person.name) enforcement gap: engine committed=%t but oracle predicted committed=%t for %q params=%v",
					engineCommitted, predicted.Committed, op.Cypher, op.Params),
			}
			return s.report(tick, op, []Violation{v}), nil
		}

		if tick%int64(s.cfg.CheckEvery) == 0 {
			if violations := s.checker.Check(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}
	}
	return nil, nil
}

// nextConstraintOp returns the next CREATE for the constraint loop: a fresh
// unique-name Person (which the UNIQUE constraint permits) or a duplicate of an
// existing name (which it must forbid). The choice and the duplicate target are
// drawn from the master seed so the op stream stays a pure function of the seed.
// *fresh is the monotone counter for fresh names.
func (s *Simulator) nextConstraintOp(fresh *int) Op {
	names := s.oracle.NodeNames()
	// ~40% duplicate attempts once names exist; always fresh while empty.
	dup := len(names) > 0 && s.seed.Float64() < 0.4
	var name string
	if dup {
		name = names[s.seed.IntN(len(names))]
	} else {
		name = fmt.Sprintf("c%d", *fresh)
		*fresh++
	}
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": int64(s.seed.IntN(100))},
	}
}
