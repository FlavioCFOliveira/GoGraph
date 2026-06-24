package sim

import (
	"context"
	"fmt"
)

// constraint_existence.go — DST scenario for NOT NULL (property-existence)
// constraint enforcement (#1754, ACID Consistency).
//
// The constraint-enforce scenario (constraint.go) covers UNIQUE. This scenario
// covers the orthogonal existence constraint: under a NOT NULL(Acct.email)
// constraint, a CREATE that carries the constrained property must commit, and a
// CREATE that OMITS it must be rejected with a typed constraint-violation error,
// applying nothing. The engine's per-op accept/reject outcome is compared against
// a locally-predicted expectation (a pure function of the op stream), and
// deterministic crash+recovery cycles prove the constraint survives recovery
// still enforcing on NEW writes. It is bit-reproducible.

// constraintExistenceDDL declares the NOT NULL (Acct, email) constraint the
// existence scenario verifies. Like the UNIQUE form it is appended to the WAL and
// fsynced (a schema change), so it survives crash/recovery and is re-registered
// on reopen via recovery.Constraints.
const constraintExistenceDDL = "CREATE CONSTRAINT sim_acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL"

// tmplCreateAcctWithEmail and tmplCreateAcctNoEmail are the two CREATE shapes the
// existence scenario interleaves: one provides the constrained property (commits)
// and one omits it (must be rejected). Distinct query texts keep each op's
// predicted outcome a pure function of the op kind, not of a runtime null check.
const (
	tmplCreateAcctWithEmail = "CREATE (n:Acct {id:$id, email:$email})"
	tmplCreateAcctNoEmail   = "CREATE (n:Acct {id:$id})"
)

// ScenarioConstraintExistence is the registry name of the existence scenario.
const ScenarioConstraintExistence = "constraint-existence"

// constraintExistenceScenario verifies NOT NULL existence-constraint enforcement
// under the DST. See the file comment for the contract.
func constraintExistenceScenario() Scenario {
	return Scenario{
		Name:        ScenarioConstraintExistence,
		Description: "NOT NULL(Acct.email) enforcement: omit-at-CREATE rejected, constraint survives crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xE4157E2C,
		MaxTicks:    500,
		// A durable store is required so the constraint (a WAL-logged schema
		// change) survives the crash cycles; crashes are moderate so several
		// recovery boundaries are exercised within the budget.
		Crash: CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:   runConstraintExistence,
	}
}

// runConstraintExistence drives the existence-enforcement run: declare the NOT
// NULL constraint, then drive a deterministic loop that interleaves valid
// (email-bearing) and violating (email-omitting) CREATEs, comparing the engine's
// accept/reject outcome to the predicted one. Crashes reuse [Simulator.maybeCrash];
// after each, the recovered engine must still enforce the constraint.
func runConstraintExistence(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := constraintExistenceScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-existence new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	if err := sm.engineRunDDL(ctx, constraintExistenceDDL); err != nil {
		return nil, fmt.Errorf("sim: constraint-existence create constraint: %w", err)
	}
	sm.Oracle().SetExistenceOnEmail(true)

	report, err := sm.runExistenceLoop(ctx)
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-existence run: %w", err)
	}
	return report, nil
}

// runExistenceLoop drives the existence-enforcement safety loop: each tick it
// emits a CREATE that is either email-bearing (must commit) or email-omitting
// (must be rejected by the NOT NULL constraint), runs it against the engine, and
// asserts the engine's accept/reject outcome matches the ORACLE's prediction. A
// mismatch — the engine accepting an omit the constraint should forbid, or
// rejecting a valid create — is an ACID_CONSISTENCY violation. The committed
// creates also advance the oracle node model, so the crash/recovery durability
// checker (which compares the recovered engine's node count to the oracle's) stays
// consistent. Crashes reuse [Simulator.maybeCrash]; post-recovery enforcement is
// verified by the same per-op comparison continuing against the recovered engine.
func (s *Simulator) runExistenceLoop(ctx context.Context) (*SimReport, error) {
	idCounter := int64(0)
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

		op := s.nextExistenceOp(&idCounter)
		engineCommitted := s.execute(ctx, op)
		// Predict via the oracle, which also advances the node model for a
		// committed create — keeping the durability checker's expected node count
		// in lock-step with the engine's durable set.
		predicted := s.oracle.ApplyCreate(op.Cypher, op.Params)
		if !engineCommitted {
			// A rejected write under this scenario is a NOT-NULL-violating omit the
			// constraint forbade; count it as the non-vacuity guard.
			s.rejectedWrites++
		}

		if engineCommitted != predicted.Committed {
			v := Violation{
				Kind: ViolationACIDConsistency,
				Tick: tick,
				Op:   "existence constraint enforcement",
				Message: fmt.Sprintf(
					"NOT NULL(Acct.email) enforcement gap: engine committed=%t but oracle predicted committed=%t for %q params=%v",
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

// nextExistenceOp returns the next CREATE for the existence loop: ~45% of the
// time an email-omitting Acct CREATE (which the NOT NULL constraint must REJECT),
// otherwise an email-bearing one (which must COMMIT). The choice is drawn from the
// master seed so the op stream stays a pure function of the seed. *id is the
// monotone counter for unique node ids.
func (s *Simulator) nextExistenceOp(id *int64) Op {
	cur := *id
	*id++
	if omit := s.seed.Float64() < 0.45; omit {
		return Op{
			Kind:   OpCreate,
			Cypher: tmplCreateAcctNoEmail,
			Params: map[string]any{"id": cur},
		}
	}
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreateAcctWithEmail,
		Params: map[string]any{"id": cur, "email": fmt.Sprintf("a%d@x", cur)},
	}
}
