package sim

// ddl_counters_oracle_test.go — coverage and TEETH for the DDL counters oracle
// (rmp #2822).
//
// The oracle exists because the simulator had no view of any DDL effect counter
// at all: [Simulator.engineRunDDL] discarded the *Result. Wiring it in is only
// half the task — the half that matters is that the gate can FAIL, so this file
// asserts both directions:
//
//   - the live sequence below drives every DDL form the engine offers through the
//     production route, including the absorbed IF EXISTS / IF NOT EXISTS no-ops,
//     and must produce no violation; and
//   - the direct arms feed [CheckDDLCounters] counter sets that lie — the
//     pre-#2818 unconditional indexes-removed among them — and each must produce
//     exactly one violation, while the truthful control produces none.
//
// The live sequence is deliberately built around the absorbed forms, because
// those are the only ones on which a counter CAN lie about a DROP: an applied
// drop and a truthful report agree trivially. It asserts the observable schema
// after every step as well, so the sequence is PROVEN to contain absorptions
// rather than assumed to.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// ddlOracleStep is one statement of the live sequence: the DDL to run, and the
// user index and constraint names that must exist afterwards, so the test can
// show which steps were absorbed no-ops.
type ddlOracleStep struct {
	ddl string
	// absorbed marks the steps that must change nothing — the IF EXISTS /
	// IF NOT EXISTS forms whose object is already in the state they ask for.
	absorbed    bool
	wantIndexes []string
	wantCons    []string
}

// ddlOracleSequence exercises CREATE and DROP, INDEX and CONSTRAINT, absorbed
// and applied, plus the two SHOW statements that change nothing at all.
var ddlOracleSequence = []ddlOracleStep{
	// THE #2818 SHAPE: dropping a name that never existed. An absorbed IF EXISTS
	// removes nothing, so it must report nothing — and this is the only step on
	// which the pre-#2818 unconditional indexes-removed bump differs from the fix.
	{ddl: `DROP INDEX ddl_idx IF EXISTS`, absorbed: true},
	{ddl: `CREATE INDEX ddl_idx FOR (n:DDLO) ON (n.k)`, wantIndexes: []string{"ddl_idx"}},
	{ddl: `CREATE INDEX IF NOT EXISTS ddl_idx FOR (n:DDLO) ON (n.k)`, absorbed: true, wantIndexes: []string{"ddl_idx"}},
	{ddl: `DROP INDEX ddl_idx IF EXISTS`},
	{ddl: `DROP INDEX ddl_idx IF EXISTS`, absorbed: true},
	{ddl: `DROP CONSTRAINT ddl_uq IF EXISTS`, absorbed: true},
	{ddl: `CREATE CONSTRAINT ddl_uq IF NOT EXISTS FOR (n:DDLO) REQUIRE n.u IS UNIQUE`, wantCons: []string{"ddl_uq"}},
	{ddl: `CREATE CONSTRAINT ddl_uq IF NOT EXISTS FOR (n:DDLO) REQUIRE n.u IS UNIQUE`, absorbed: true, wantCons: []string{"ddl_uq"}},
	{ddl: `DROP CONSTRAINT ddl_uq IF EXISTS`},
	{ddl: `DROP CONSTRAINT ddl_uq IF EXISTS`, absorbed: true},
	// A NOT NULL constraint has no backing index, so it is a second constraint
	// shape rather than a repeat of the first.
	{ddl: `CREATE CONSTRAINT ddl_nn FOR (n:DDLO) REQUIRE n.r IS NOT NULL`, wantCons: []string{"ddl_nn"}},
	{ddl: `DROP CONSTRAINT ddl_nn`},
	// Pure reads: they carry no write surface and must report no effect.
	{ddl: `SHOW INDEXES`, absorbed: true},
	{ddl: `SHOW CONSTRAINTS`, absorbed: true},
}

// TestDDLCountersOracle_EveryFormAgrees drives [ddlOracleSequence] through the
// production route — [Simulator.engineRunDDL], which is what every scenario
// calls — and requires every statement to pass the oracle.
//
// On a build carrying the pre-#2818 defect the FIRST step fails: the absorbed
// `DROP INDEX ddl_idx IF EXISTS` reports indexesRemoved: 1 while the engine's
// index registry is identical before and after.
func TestDDLCountersOracle_EveryFormAgrees(t *testing.T) {
	t.Parallel()
	sm, err := New(Config{Seed: 0x2822, MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	absorbedSeen, appliedSeen := 0, 0
	for _, step := range ddlOracleSequence {
		before := readDDLSchemaNames(sm.engine)
		if err := sm.engineRunDDL(ctx, step.ddl); err != nil {
			t.Fatalf("%s: %v", step.ddl, err)
		}
		after := readDDLSchemaNames(sm.engine)

		// Prove the step really was what the table says it was, rather than
		// trusting the label: an absorbed step must move nothing.
		changed := len(after.indexes) != len(before.indexes) || len(after.constraints) != len(before.constraints)
		if step.absorbed && changed {
			t.Fatalf("%s is declared absorbed but changed the schema", step.ddl)
		}
		if !step.absorbed && !changed {
			t.Fatalf("%s is declared applied but changed nothing", step.ddl)
		}
		if step.absorbed {
			absorbedSeen++
		} else {
			appliedSeen++
		}
		assertNameSet(t, step.ddl, "indexes", after.indexes, step.wantIndexes)
		assertNameSet(t, step.ddl, "constraints", after.constraints, step.wantCons)
	}
	// NON-VACUITY of the sequence itself: both branches of the oracle's
	// derivation must have been walked.
	if absorbedSeen == 0 || appliedSeen == 0 {
		t.Fatalf("sequence walked %d absorbed and %d applied steps; both must be non-zero",
			absorbedSeen, appliedSeen)
	}
}

// assertNameSet checks that got holds exactly the names in want.
func assertNameSet(t *testing.T, ddl, what string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("after %s the engine holds %d %s, want %d (%v)", ddl, len(got), what, len(want), want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("after %s the engine does not hold %s %q", ddl, what, name)
		}
	}
}

// ddlNames builds a [ddlSchemaNames] from two literal name lists.
func ddlNames(indexes, constraints []string) ddlSchemaNames {
	s := ddlSchemaNames{indexes: map[string]struct{}{}, constraints: map[string]struct{}{}}
	for _, n := range indexes {
		s.indexes[n] = struct{}{}
	}
	for _, n := range constraints {
		s.constraints[n] = struct{}{}
	}
	return s
}

// TestDDLCountersOracle_HasTeeth feeds the checker counter sets that lie, one per
// way a DDL statement can misreport, and requires each to be caught — with a
// truthful control for every one of them, so the checker is not simply
// condemning everything it is shown.
func TestDDLCountersOracle_HasTeeth(t *testing.T) {
	t.Parallel()
	noIndexes := ddlNames(nil, nil)
	oneIndex := ddlNames([]string{"i1"}, nil)
	oneCons := ddlNames(nil, []string{"c1"})

	cases := []struct {
		name          string
		before, after ddlSchemaNames
		got           *exec.QueryCounters
		wantViolation bool
	}{
		{
			// The defect rmp #2818 fixed, and the reason this oracle exists: the
			// index never existed, the registry is identical either side, and the
			// statement claims a removal.
			name: "Pre2818UnconditionalIndexesRemoved", before: noIndexes, after: noIndexes,
			got: &exec.QueryCounters{IndexesRemoved: 1}, wantViolation: true,
		},
		{
			// Control for it: a drop that really removed the index reports the
			// same counter and must pass.
			name: "TruthfulIndexesRemoved", before: oneIndex, after: noIndexes,
			got: &exec.QueryCounters{IndexesRemoved: 1}, wantViolation: false,
		},
		{
			name: "AbsorbedDropReportsNothing", before: noIndexes, after: noIndexes,
			got: nil, wantViolation: false,
		},
		{
			// The mirror of #2818 on the CREATE side.
			name: "UnconditionalIndexesAdded", before: oneIndex, after: oneIndex,
			got: &exec.QueryCounters{IndexesAdded: 1}, wantViolation: true,
		},
		{
			name: "UnconditionalConstraintsRemoved", before: noIndexes, after: noIndexes,
			got: &exec.QueryCounters{ConstraintsRemoved: 1}, wantViolation: true,
		},
		{
			name: "TruthfulConstraintsRemoved", before: oneCons, after: noIndexes,
			got: &exec.QueryCounters{ConstraintsRemoved: 1}, wantViolation: false,
		},
		{
			// An applied effect the statement did not report at all: nil counters
			// across a registry that gained an index.
			name: "AppliedEffectUnreported", before: noIndexes, after: oneIndex,
			got: nil, wantViolation: true,
		},
		{
			// An applied effect reported as the WRONG kind.
			name: "IndexEffectReportedAsConstraint", before: noIndexes, after: oneIndex,
			got: &exec.QueryCounters{ConstraintsAdded: 1}, wantViolation: true,
		},
		{
			// A DDL statement writes no data, so a data counter on one is a leak.
			name: "DataEffectOnDDL", before: noIndexes, after: oneIndex,
			got: &exec.QueryCounters{IndexesAdded: 1, NodesCreated: 1}, wantViolation: true,
		},
		{
			name: "TruthfulIndexesAdded", before: noIndexes, after: oneIndex,
			got: &exec.QueryCounters{IndexesAdded: 1}, wantViolation: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckDDLCounters(7, "<probe>", tc.before, tc.after, tc.got)
			if tc.wantViolation && len(v) == 0 {
				t.Fatalf("counters %v across before=%v after=%v raised no violation",
					tc.got, tc.before.indexes, tc.after.indexes)
			}
			if !tc.wantViolation && len(v) > 0 {
				t.Fatalf("truthful counters raised %d violation(s): %v", len(v), v)
			}
			if tc.wantViolation {
				if v[0].Kind != ViolationOracleDeviation {
					t.Errorf("violation kind = %s, want %s", v[0].Kind, ViolationOracleDeviation)
				}
				if v[0].Tick != 7 {
					t.Errorf("violation tick = %d, want 7", v[0].Tick)
				}
			}
		})
	}
}

// TestDDLCountersOracle_IgnoresInternalIndexes pins the name filter the
// derivation depends on: a UNIQUE constraint brings its "__uniq__" backing index
// and a user index brings its "_btree_num" numeric companion, and counting
// either would attribute an index effect to a constraint statement, or two index
// effects to one CREATE INDEX.
func TestDDLCountersOracle_IgnoresInternalIndexes(t *testing.T) {
	t.Parallel()
	sm, err := New(Config{Seed: 0x2822, MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	// A numeric property is what makes the engine build the "_btree_num"
	// companion, so the fixture seeds one row before creating the index.
	if err := runSimWrite(ctx, sm, `CREATE (:DDLI {k: 1})`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, ddl := range []string{
		`CREATE INDEX ddli_idx FOR (n:DDLI) ON (n.k)`,
		`CREATE CONSTRAINT ddli_uq FOR (n:DDLI) REQUIRE n.u IS UNIQUE`,
	} {
		if err := sm.engineRunDDL(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	// The raw registry holds the internal names; the oracle's view holds only
	// the one user index. If it did not, engineRunDDL above would already have
	// failed — this asserts WHY it passed.
	raw := sm.engine.ListIndexes()
	if len(raw) < 3 {
		t.Fatalf("engine holds %d indexes (%v); the fixture must produce a user index, "+
			"its numeric companion and the UNIQUE backing index", len(raw), raw)
	}
	filtered := readDDLSchemaNames(sm.engine)
	assertNameSet(t, "<fixture>", "indexes", filtered.indexes, []string{"ddli_idx"})
	assertNameSet(t, "<fixture>", "constraints", filtered.constraints, []string{"ddli_uq"})
}

// runSimWrite executes one data write against the simulator's engine, for a
// fixture that needs rows before its DDL.
func runSimWrite(ctx context.Context, sm *Simulator, query string) error {
	res, err := sm.engine.RunWrite(ctx, query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	err = res.Err()
	_ = res.Close()
	return err
}
