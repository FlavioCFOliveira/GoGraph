package sim

import (
	"context"
	"testing"
)

// TestSchemaMutation_Scenario_Passes runs the registered schema-mutation
// scenario: REMOVE property, REMOVE label, SET label, SET n += $map and
// SET n = $map must each round-trip and survive crash + checkpoint recovery.
// A nil report means every mutation held on every check, including post-recovery.
func TestSchemaMutation_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioSchemaMutation)
	if !ok {
		t.Fatalf("schema-mutation scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("schema-mutation run: %v", err)
	}
	if report != nil {
		t.Fatalf("schema-mutation reported a violation (a mutation did not round-trip/survive):\n%s", report)
	}
}

// TestSchemaMutation_MultiSeed runs a few extra seeds so the op mix is exercised
// across different interleavings of create/remove/set-label/set-map.
func TestSchemaMutation_MultiSeed(t *testing.T) {
	for _, seed := range []uint64{1, 2, 0xABCDEF, 0x5C4E3A17} {
		report, err := runSchemaMutation(context.Background(), seed)
		if err != nil {
			t.Fatalf("runSchemaMutation(%#x): %v", seed, err)
		}
		if report != nil {
			t.Fatalf("runSchemaMutation(%#x) violation:\n%s", seed, report)
		}
	}
}

// TestSchemaMutation_Reproducible confirms the scenario is bit-reproducible: the
// same seed drives the identical op stream, so two runs reach the same outcome.
func TestSchemaMutation_Reproducible(t *testing.T) {
	r1, err1 := runSchemaMutation(context.Background(), 0x5C4E3A17)
	r2, err2 := runSchemaMutation(context.Background(), 0x5C4E3A17)
	if err1 != nil || err2 != nil {
		t.Fatalf("run errors: %v / %v", err1, err2)
	}
	if (r1 == nil) != (r2 == nil) {
		t.Fatalf("non-reproducible outcome: r1=%v r2=%v", r1, r2)
	}
}

// TestSchemaMutation_CheckerCatchesDivergence is the meta-test: it proves
// CheckSchemaMutation CATCHES both a property and a label divergence, so a clean
// run is genuinely evidence the mutations round-tripped. It also exercises the
// oracle's applySchemaMutation modelling of each template.
func TestSchemaMutation_CheckerCatchesDivergence(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	ctx := context.Background()

	// Create a Person in both the engine and the oracle.
	if _, err := a.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": "Ada", "age": int64(36)}); err != nil {
		t.Fatalf("seed Person: %v", err)
	}
	oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "Ada", "age": int64(36)})

	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("faithful model should be clean, got: %v", v)
	}

	// Property divergence: oracle claims a tag the engine never set.
	id := oracle.byName["Ada"]
	oracle.nodes[id].Properties["tag"] = "ghost"
	if v := CheckSchemaMutation(0, oracle, a); len(v) == 0 {
		t.Fatalf("checker missed a property divergence")
	}
	delete(oracle.nodes[id].Properties, "tag") // restore

	// Label divergence: oracle claims Vip but the engine node has no Vip label.
	oracle.addLabel(id, "Vip")
	if v := CheckSchemaMutation(0, oracle, a); len(v) == 0 {
		t.Fatalf("checker missed a label divergence")
	}
}

// TestSchemaMutation_OracleModelsMutations verifies the oracle's
// applySchemaMutation round-trips against the engine for each template on one
// node — the positive counterpart to the divergence meta-test.
func TestSchemaMutation_OracleModelsMutations(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	ctx := context.Background()

	name := "Grace"
	create := func(q string, p map[string]any, apply func()) {
		t.Helper()
		if _, err := a.RunWrite(ctx, q, p); err != nil {
			t.Fatalf("engine %q: %v", q, err)
		}
		apply()
		if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
			t.Fatalf("after %q: engine and oracle diverged: %v", q, v)
		}
	}

	create(tmplCreatePerson, map[string]any{"name": name, "age": int64(50)},
		func() { oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": name, "age": int64(50)}) })
	create(tmplSetTag, map[string]any{"name": name, "tag": "t1"},
		func() { oracle.ApplyMatch(tmplSetTag, map[string]any{"name": name, "tag": "t1"}) })
	create(tmplAddVip, map[string]any{"name": name},
		func() { oracle.ApplyMatch(tmplAddVip, map[string]any{"name": name}) })
	create(tmplMergeProps, map[string]any{"name": name, "props": map[string]any{"score": int64(7)}},
		func() { oracle.ApplyMatch(tmplMergeProps, map[string]any{"name": name, "props": map[string]any{"score": int64(7)}}) })
	create(tmplRemoveTag, map[string]any{"name": name},
		func() { oracle.ApplyMatch(tmplRemoveTag, map[string]any{"name": name}) })
	create(tmplRemoveVip, map[string]any{"name": name},
		func() { oracle.ApplyMatch(tmplRemoveVip, map[string]any{"name": name}) })
	// SET n = $props replaces the property set (drops score), keeps name+age.
	create(tmplReplaceProps, map[string]any{"name": name, "props": map[string]any{"name": name, "age": int64(51)}},
		func() {
			oracle.ApplyMatch(tmplReplaceProps, map[string]any{"name": name, "props": map[string]any{"name": name, "age": int64(51)}})
		})
}
