package sim

import (
	"context"
	"strings"
	"testing"
)

// schema_introspect_test.go — happy-path and sensitivity proofs for the
// schema-introspection oracle, the modern/legacy constraint-grammar
// equivalence, and the constraint-kind non-vacuity gate (rmp #2455).

// newIntrospectionSim builds a lightweight (in-memory, crash-free) simulator
// and applies the given DDL statements to its engine.
func newIntrospectionSim(t *testing.T, ddls ...string) *Simulator {
	t.Helper()
	sm, err := New(Config{Seed: 0x5C4E, MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	for _, ddl := range ddls {
		if err := sm.engineRunDDL(context.Background(), ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	return sm
}

// introspectionFixtureDDL declares two user indexes (hash and btree) and two
// constraints — the UNIQUE one in the LEGACY ON ... ASSERT grammar and the
// NOT NULL one in the MODERN FOR ... REQUIRE grammar — so the happy-path
// fixture covers both index kinds, both constraint kinds, both grammars, the
// UNIQUE backing-index row, and non-empty YIELD projections.
var introspectionFixtureDDL = []string{
	"CREATE INDEX fix_name_idx FOR (n:Person) ON (n.name) OPTIONS {indexType:'hash'}",
	"CREATE INDEX fix_age_idx FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}",
	"CREATE CONSTRAINT fix_email_uq ON (n:Acct) ASSERT n.email IS UNIQUE",
	"CREATE CONSTRAINT fix_phone_nn FOR (n:Acct) REQUIRE n.phone IS NOT NULL",
}

// introspectionFixtureModel returns the SchemaModel matching
// [introspectionFixtureDDL].
func introspectionFixtureModel() *SchemaModel {
	m := NewSchemaModel()
	m.AddIndex("fix_name_idx", SchemaIndexHash, "Person", "name")
	m.AddIndex("fix_age_idx", SchemaIndexBTree, "Person", "age")
	m.AddUniqueConstraint("fix_email_uq", "Acct", "email")
	m.AddNotNullConstraint("fix_phone_nn", "Acct", "phone")
	return m
}

// TestCheckSchemaIntrospection_HappyPath: with a model that exactly matches
// the issued DDL, the oracle must report nothing — across SHOW INDEXES,
// SHOW CONSTRAINTS, db.indexes(), db.constraints(), the cross-surface
// agreement, both YIELD/WHERE/RETURN projections, and the
// db.schema.visualization() drain.
func TestCheckSchemaIntrospection_HappyPath(t *testing.T) {
	sm := newIntrospectionSim(t, introspectionFixtureDDL...)
	if v := CheckSchemaIntrospection(0, introspectionFixtureModel(), sm.engine); len(v) > 0 {
		t.Fatalf("happy path reported violations:\n%v", v)
	}
}

// TestCheckSchemaIntrospection_PhantomIndexInModelFires: an index the model
// claims but the engine never created must fire on BOTH index surfaces
// (SHOW INDEXES vs model, db.indexes() vs model).
func TestCheckSchemaIntrospection_PhantomIndexInModelFires(t *testing.T) {
	sm := newIntrospectionSim(t, introspectionFixtureDDL...)
	model := introspectionFixtureModel()
	model.AddIndex("phantom_idx", SchemaIndexHash, "Ghost", "prop")

	v := CheckSchemaIntrospection(0, model, sm.engine)
	if len(v) == 0 {
		t.Fatal("phantom index in the model was not detected: the introspection oracle is blind")
	}
	assertViolationMentions(t, v, "SHOW INDEXES")
	assertViolationMentions(t, v, "db.indexes()")
}

// TestCheckSchemaIntrospection_MissingConstraintInModelFires: a constraint the
// engine holds but the model omits must fire on BOTH constraint surfaces
// (SHOW CONSTRAINTS vs model, db.constraints() vs model).
func TestCheckSchemaIntrospection_MissingConstraintInModelFires(t *testing.T) {
	sm := newIntrospectionSim(t, introspectionFixtureDDL...)
	model := introspectionFixtureModel()
	model.DropConstraint("fix_phone_nn") // the engine still holds it

	v := CheckSchemaIntrospection(0, model, sm.engine)
	if len(v) == 0 {
		t.Fatal("missing constraint in the model was not detected: the introspection oracle is blind")
	}
	assertViolationMentions(t, v, "SHOW CONSTRAINTS")
	assertViolationMentions(t, v, "db.constraints()")
}

// assertViolationMentions asserts at least one violation message references
// the given surface, so the sensitivity proof shows WHICH check fired rather
// than merely that something did.
func assertViolationMentions(t *testing.T, vs []Violation, surface string) {
	t.Helper()
	for _, v := range vs {
		if strings.Contains(v.Message, surface) {
			return
		}
	}
	t.Errorf("no violation mentions %q; got:\n%v", surface, vs)
}

// TestSchemaChanger_ModernLegacyFormsIntrospectionIdentical proves the two
// constraint grammars the SchemaChanger alternates are introspection-
// equivalent: creating the constraint with the legacy ON ... ASSERT statement
// and with the modern FOR ... REQUIRE statement must produce byte-identical
// SHOW CONSTRAINTS and SHOW INDEXES row sets (same name, kind, label,
// property, and backing index) — and those row sets must be non-empty, so the
// equivalence is not vacuous.
func TestSchemaChanger_ModernLegacyFormsIntrospectionIdentical(t *testing.T) {
	sm := newIntrospectionSim(t)
	ctx := context.Background()
	a := SchemaChanger{}

	capture := func() ([]string, []string) {
		t.Helper()
		cons, err := sm.engine.queryRowStrings(ctx, queryShowConstraints, 5)
		if err != nil {
			t.Fatalf("SHOW CONSTRAINTS: %v", err)
		}
		idx, err := sm.engine.queryRowStrings(ctx, queryShowIndexes, 6)
		if err != nil {
			t.Fatalf("SHOW INDEXES: %v", err)
		}
		return joinRows(cons), joinRows(idx)
	}

	if err := sm.engineRunDDL(ctx, a.statement(SchemaCreateConstraint, false)); err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	legacyCons, legacyIdx := capture()
	if len(legacyCons) == 0 || len(legacyIdx) == 0 {
		t.Fatalf("legacy grammar produced empty introspection rows (cons=%d idx=%d): vacuous equivalence", len(legacyCons), len(legacyIdx))
	}

	if err := sm.engineRunDDL(ctx, "DROP CONSTRAINT "+schemaConstraintName+" IF EXISTS"); err != nil {
		t.Fatalf("drop between grammars: %v", err)
	}
	if err := sm.engineRunDDL(ctx, a.statement(SchemaCreateConstraint, true)); err != nil {
		t.Fatalf("modern create: %v", err)
	}
	modernCons, modernIdx := capture()

	if strings.Join(legacyCons, "\n") != strings.Join(modernCons, "\n") {
		t.Errorf("SHOW CONSTRAINTS differs between grammars:\nlegacy:\n  %s\nmodern:\n  %s",
			strings.Join(legacyCons, "\n  "), strings.Join(modernCons, "\n  "))
	}
	if strings.Join(legacyIdx, "\n") != strings.Join(modernIdx, "\n") {
		t.Errorf("SHOW INDEXES differs between grammars:\nlegacy:\n  %s\nmodern:\n  %s",
			strings.Join(legacyIdx, "\n  "), strings.Join(modernIdx, "\n  "))
	}
}

// TestConstraintEnforce_NonVacuityGateWired proves the terminal
// assert-something-was-seen gate is actually wired into the scenario run: a
// budget too small to exercise every constraint-kind arm must yield a
// violation report naming the vacuous arm, not a silent pass.
func TestConstraintEnforce_NonVacuityGateWired(t *testing.T) {
	sc := constraintEnforceScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = 8 // far too few ticks for all twelve route/outcome arms

	report, err := runConstraintEnforceCfg(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runConstraintEnforceCfg: %v", err)
	}
	if report == nil {
		t.Fatal("an 8-tick run passed silently: the non-vacuity gate is not wired")
	}
	found := false
	for _, v := range report.Violations {
		if v.Op == "constraint-kind non-vacuity" && strings.Contains(v.Message, "never occurred") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a constraint-kind non-vacuity violation, got:\n%s", report)
	}
}
