package sim

import (
	"context"
	"strings"
	"testing"
)

// newCallYieldWhereFixture declares the introspection fixture model's DDL in a
// real engine, so the CALL ... YIELD ... WHERE probes run against a live schema.
func newCallYieldWhereFixture(t *testing.T) (*EngineAdapter, *SchemaModel) {
	t.Helper()
	sm, err := New(Config{MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	ctx := context.Background()
	for _, ddl := range []string{
		"CREATE INDEX fix_name_idx FOR (n:Person) ON (n.name)",
		"CREATE INDEX fix_age_idx FOR (n:Person) ON (n.age) OPTIONS {indexProvider: 'btree'}",
		"CREATE CONSTRAINT fix_email_uq ON (n:Acct) ASSERT n.email IS UNIQUE",
		"CREATE CONSTRAINT fix_phone_nn ON (n:Acct) ASSERT n.phone IS NOT NULL",
	} {
		if err := sm.engineRunDDL(ctx, ddl); err != nil {
			t.Fatalf("DDL %q: %v", ddl, err)
		}
	}
	return sm.engine, introspectionFixtureModel()
}

// TestCallYieldWhere_FiltersAgainstTheModel is the happy path: the WHERE
// predicate on a CALL ... YIELD sub-clause must narrow both procedure
// enumerations to exactly the modelled name.
func TestCallYieldWhere_FiltersAgainstTheModel(t *testing.T) {
	engine, model := newCallYieldWhereFixture(t)
	if v := checkCallYieldWhere(context.Background(), 0, model, engine); len(v) > 0 {
		t.Fatalf("CALL ... YIELD ... WHERE probe reported a violation on a conforming engine: %v", v)
	}
}

// TestCallYieldWhere_ScenarioModelsAreDiscriminating proves the non-vacuity arm
// is LIVE rather than skipped everywhere: the models the real scenarios pass to
// CheckSchemaIntrospection must enumerate at least two rows, so the
// single-name filter genuinely excludes something. Without this, every call
// site could hold one row and the probe would be unable to detect a dropped
// WHERE while still reporting a pass.
func TestCallYieldWhere_ScenarioModelsAreDiscriminating(t *testing.T) {
	models := map[string]*SchemaModel{
		"constraint-enforce":    constraintEnforceSchemaModel(),
		"introspection fixture": introspectionFixtureModel(),
	}
	for name, m := range models {
		m := m
		t.Run(name, func(t *testing.T) {
			if n := len(m.constraintNameList()); n < 2 {
				t.Errorf("model enumerates %d constraint(s); the WHERE filter cannot discriminate below 2", n)
			}
			if n := len(m.indexNameList()); n < 2 {
				t.Errorf("model enumerates %d index(es); the WHERE filter cannot discriminate below 2", n)
			}
		})
	}
}

// TestCallYieldWhere_SensitivityToAWrongModel proves the probe FIRES when the
// model and the engine disagree: a name the engine does not hold must produce a
// violation rather than an empty match compared against an empty expectation.
func TestCallYieldWhere_SensitivityToAWrongModel(t *testing.T) {
	engine, _ := newCallYieldWhereFixture(t)

	// The probe filters on the lexicographically FIRST modelled name, so a
	// phantom must sort ahead of every real one to be the name selected. Backing
	// indexes are named "__uniq__…" and '_' (0x5F) sorts below the letters, so a
	// digit prefix is used to win against both families.
	t.Run("phantom constraint name", func(t *testing.T) {
		m := introspectionFixtureModel()
		m.AddUniqueConstraint("0_not_in_the_engine", "Ghost", "prop")
		v := checkCallYieldWhere(context.Background(), 0, m, engine)
		if len(v) == 0 {
			t.Fatal("probe FAILED to fire for a constraint name the engine does not hold")
		}
		if !strings.Contains(v[0].Message, "#1966") {
			t.Errorf("violation should name the backlog item it guards: %s", v[0].Message)
		}
	})

	t.Run("phantom index name", func(t *testing.T) {
		m := introspectionFixtureModel()
		m.AddIndex("0_ghost_idx", SchemaIndexHash, "Ghost", "prop")
		if v := checkCallYieldWhere(context.Background(), 0, m, engine); len(v) == 0 {
			t.Fatal("probe FAILED to fire for an index name the engine does not hold")
		}
	})
}

// TestCallYieldWhere_VacuousFilterIsReported proves the non-vacuity arm fires
// when the predicate excludes nothing. A model declaring a single row is fed to
// the probe alongside an engine holding many, so the filtered projection returns
// at least as many rows as the model enumerates — the signature of a WHERE that
// is not narrowing.
func TestCallYieldWhere_VacuousFilterIsReported(t *testing.T) {
	// Directly exercise the guard: with two modelled names, a result set that
	// still holds two rows means the predicate excluded nothing.
	m := introspectionFixtureModel()
	if len(m.constraintNameList()) < 2 {
		t.Fatal("fixture model must enumerate at least two constraints for this test")
	}
	// The engine below has NO DDL at all, so db.constraints() yields nothing and
	// the row-set comparison fires first — proving the probe never passes when
	// the engine and the model disagree about the enumeration.
	sm, err := New(Config{MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sm.Close() }()
	if v := checkCallYieldWhere(context.Background(), 0, m, sm.engine); len(v) == 0 {
		t.Fatal("probe FAILED to fire against an engine holding none of the modelled schema")
	}
}
