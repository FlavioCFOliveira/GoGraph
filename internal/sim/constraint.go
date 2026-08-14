package sim

import (
	"context"
	"fmt"
)

// constraintEnforceDDL declares the UNIQUE (Person, name) constraint the
// constraint-enforcement scenario verifies, deliberately in the LEGACY
// ON ... ASSERT grammar (cypher/ir/ddl_parser.go); the constraint is appended
// to the WAL and fsynced, so it survives crash/recovery and is re-registered
// on reopen.
const constraintEnforceDDL = "CREATE CONSTRAINT sim_person_name_unique ON (n:Person) ASSERT n.name IS UNIQUE"

// constraintNumDDL declares the numeric UNIQUE (Num, val) constraint behind
// the constraint-kind battery (rmp #2455), deliberately in the MODERN
// FOR ... REQUIRE grammar, so both constraint grammars run — and recover —
// under the same deterministic scenario.
const constraintNumDDL = "CREATE CONSTRAINT sim_num_val_unique FOR (n:Num) REQUIRE n.val IS UNIQUE"

// constraintEnforceSchemaModel returns the scenario's DDL model for the
// schema-introspection oracle: both UNIQUE constraints (each implying its hash
// backing index in the SHOW INDEXES / db.indexes() enumeration).
func constraintEnforceSchemaModel() *SchemaModel {
	m := NewSchemaModel()
	m.AddUniqueConstraint("sim_person_name_unique", "Person", "name")
	m.AddUniqueConstraint("sim_num_val_unique", "Num", "val")
	return m
}

// constraintEnforceScenario verifies UNIQUE constraint enforcement under the
// DST across every engine-supported route into the constraint (rmp #2455): it
// creates a UNIQUE (Person, name) constraint (legacy grammar) and a numeric
// UNIQUE (Num, val) constraint (modern grammar), then drives a workload
// interleaving, per route, writes that must commit with writes the engine MUST
// reject with a typed constraint-violation error, applying nothing —
// duplicate-name CREATEs, cross-node renames via SET n.name, MERGE ... ON
// CREATE SET duplicates, SET-label promotions of colliding nodes, and numeric
// duplicates including float spellings of held integer values. A local
// prediction adjudicates every outcome; a disagreement is an enforcement gap.
// Deterministic crash+recovery cycles prove both constraints survive recovery
// still enforcing, the schema-introspection oracle holds SHOW / db.* to the
// declared DDL after every recovery, and a terminal non-vacuity gate requires
// every route/outcome arm to have actually occurred. It is bit-reproducible.
func constraintEnforceScenario() Scenario {
	return Scenario{
		Name:        ScenarioConstraintEnforce,
		Description: "UNIQUE enforcement on every route (CREATE, SET rename, MERGE ON CREATE, SET label, numeric key): violations rejected, constraints + introspection survive crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xC047A157,
		MaxTicks:    500,
		// A durable store is required so the constraints (WAL-logged schema
		// changes) survive the crash cycles; crashes are moderate so several
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
// check); after each the constraints must still be enforced.
func runConstraintEnforce(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := constraintEnforceScenario()
	return runConstraintEnforceCfg(ctx, sc.DeterministicConfig(seed))
}

// runConstraintEnforceCfg is [runConstraintEnforce] over an explicit [Config],
// split out so tests can prove the terminal non-vacuity gate is wired: a
// config whose budget cannot exercise every constraint-kind arm must yield a
// violation report, not a silent pass.
func runConstraintEnforceCfg(ctx context.Context, cfg Config) (*SimReport, error) {
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	// Declare both UNIQUE constraints in the engine and model the Person one in
	// the oracle (the numeric one is modelled by the constraint-kind state).
	if err := sm.engineRunDDL(ctx, constraintEnforceDDL); err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce create constraint: %w", err)
	}
	if err := sm.engineRunDDL(ctx, constraintNumDDL); err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce create numeric constraint: %w", err)
	}
	sm.Oracle().SetUniqueOnName(true)

	report, err := sm.runConstraintLoop(ctx, constraintEnforceSchemaModel())
	if err != nil {
		return nil, fmt.Errorf("sim: constraint-enforce run: %w", err)
	}
	return report, nil
}

// runConstraintLoop drives the constraint-enforcement safety loop: each tick it
// emits one constraint-kind op (see constraint_kinds.go — CREATE, SET rename,
// MERGE ... ON CREATE SET, Plain create, SET-label promote, or numeric create),
// runs it against the engine, and asserts the engine's accept/reject outcome
// matches the prediction. A mismatch — the engine accepting a write a
// constraint should forbid, or rejecting a valid one — is an ACID_CONSISTENCY
// violation (the engine failed to enforce a declared invariant). Crashes reuse
// [Simulator.maybeCrash]; post-recovery enforcement is verified by the same
// per-op comparison continuing against the recovered engine, and — when model
// is non-nil — the schema-introspection oracle ([CheckSchemaIntrospection],
// rmp #2455) holds SHOW CONSTRAINTS / SHOW INDEXES and db.constraints() /
// db.indexes() to the declared DDL at the start, after every recovery, and at
// the end. A nil model skips the introspection checks, so the
// enforcement-gap meta-tests (which deliberately declare NO DDL in the
// engine) can isolate the per-op adjudicator. A terminal non-vacuity gate
// requires every route/outcome arm to have occurred.
func (s *Simulator) runConstraintLoop(ctx context.Context, model *SchemaModel) (*SimReport, error) {
	st := newConstraintKindState()
	if model != nil {
		if v := CheckSchemaIntrospection(0, model, s.engine); len(v) > 0 {
			return s.report(0, Op{Kind: OpMatch, Cypher: "<initial schema introspection>"}, v), nil
		}
	}

	var lastTick int64
	var lastOp Op
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		crashesBefore := s.crashCount
		if report, err := s.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if s.crashCount > crashesBefore && model != nil {
			// Recovered-DDL introspection: both constraints (and the backing
			// indexes) must re-register with the same names, kinds, and shapes.
			if v := CheckSchemaIntrospection(tick, model, s.engine); len(v) > 0 {
				return s.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery schema introspection>"}, v), nil
			}
		}

		op := st.nextOp(s.seed, s.oracle)
		engineCommitted := s.execute(ctx, op)
		predicted := st.apply(s.oracle, op)
		if !engineCommitted {
			// A rejected write under this scenario is a UNIQUE-violating write a
			// constraint forbade; count it as the non-vacuity guard.
			s.rejectedWrites++
		}

		if engineCommitted != predicted.Committed {
			v := Violation{
				Kind: ViolationACIDConsistency,
				Tick: tick,
				Op:   "constraint enforcement",
				Message: fmt.Sprintf(
					"UNIQUE enforcement gap: engine committed=%t but oracle predicted committed=%t for %q params=%v",
					engineCommitted, predicted.Committed, op.Cypher, op.Params),
			}
			return s.report(tick, op, []Violation{v}), nil
		}
		st.note(op, engineCommitted)
		lastTick, lastOp = tick, op

		if tick%int64(s.cfg.CheckEvery) == 0 {
			if violations := s.checker.Check(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}
	}
	if model != nil {
		if v := CheckSchemaIntrospection(lastTick, model, s.engine); len(v) > 0 {
			return s.report(lastTick, Op{Kind: OpMatch, Cypher: "<terminal schema introspection>"}, v), nil
		}
	}
	// Assert-something-was-seen (rmp #2455): every constraint route, in both
	// its commit and reject arm, must have actually run.
	if v := st.checkNonVacuity(lastTick); len(v) > 0 {
		return s.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
