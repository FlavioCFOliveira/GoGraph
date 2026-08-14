package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildNullSemanticsFixture creates a hand-analysable NULL-semantics graph in
// BOTH the engine and the oracle: age-bearing Persons a1(25), a2(31), a3(40),
// a4(30) and AGELESS Persons n1, n2, n3, with KNOWS edges n1→a1, n1→a2 (an
// ageless source with two targets), a3→n2 (an ageless node as target only),
// and a4→a1 (aged-to-aged, invisible to the OPTIONAL MATCH probe).
func buildNullSemanticsFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	a := NewEngineAdapter(cypher.NewEngine(g))
	o := NewGraphOracle()
	ctx := context.Background()
	apply := func(cypher string, params map[string]any) {
		t.Helper()
		if _, err := a.RunWrite(ctx, cypher, params); err != nil {
			t.Fatalf("fixture write %q: %v", cypher, err)
		}
		o.ApplyCreate(cypher, params)
	}
	apply(tmplCreatePerson, map[string]any{"name": "a1", "age": int64(25)})
	apply(tmplCreatePersonCity, map[string]any{"name": "a2", "age": int64(31), "city": "c1"})
	apply(tmplCreatePersonCity, map[string]any{"name": "a3", "age": int64(40), "city": "c2"})
	apply(tmplCreatePerson, map[string]any{"name": "a4", "age": int64(30)})
	for i, city := range []string{"c1", "c3", "c4"} {
		apply(tmplCreatePersonNoAge, map[string]any{"name": "n" + string(rune('1'+i)), "city": city})
	}
	for _, e := range [][2]string{{"n1", "a1"}, {"n1", "a2"}, {"a3", "n2"}, {"a4", "a1"}} {
		apply(tmplCreateKnows, map[string]any{"a": e[0], "b": e[1]})
	}
	return a, o
}

// TestNullSemantics_Fixture_HandVerified pins the oracle's age partition and
// the 3VL partition counts to values derived BY HAND on the fixture, then
// asserts the full battery and the non-vacuity assertion pass against the
// untouched engine. Hand derivation over ages {25, 31, 40, 30}:
//
//   - total = 7, age-bearing = 4, ageless = 3 (n1, n2, n3);
//   - age > 30 → {31, 40} = 2; NOT (age > 30) → {25, 30} = 2; IS NULL = 3;
//     the partition identity: 2 + 2 + 3 = 7;
//   - sum = 126, min = 25, max = 40; coalesce(age, -1) sum = 126 - 3 = 123;
//   - OPTIONAL MATCH rows: (n1, a1), (n1, a2), (n2, null), (n3, null).
func TestNullSemantics_Fixture_HandVerified(t *testing.T) {
	a, o := buildNullSemanticsFixture(t)

	withAge, ageless := o.personAgePartition()
	if len(withAge) != 4 || len(ageless) != 3 {
		t.Fatalf("age partition disagrees with the hand derivation: withAge=%v ageless=%v", withAge, ageless)
	}
	var gt30, le30 int
	for _, age := range o.personAges() {
		if age > 30 {
			gt30++
		} else {
			le30++
		}
	}
	if gt30 != 2 || le30 != 2 {
		t.Fatalf("3VL partition disagrees with the hand derivation: gt30=%d le30=%d, want 2 and 2", gt30, le30)
	}
	if gt30+le30+len(ageless) != len(withAge)+len(ageless) {
		t.Fatalf("hand-derived partition identity broken: %d + %d + %d != %d", gt30, le30, len(ageless), 7)
	}

	if v := CheckNullSemantics(0, o, a); len(v) > 0 {
		t.Fatalf("engine disagrees with the hand-verified references: %v", v)
	}
	if v := checkNullSemanticsNonVacuity(0, o); len(v) > 0 {
		t.Fatalf("fixture populates every probe shape, non-vacuity must not fire: %v", v)
	}
}

// distinctOps returns how many distinct probe labels (Violation.Op) appear in v.
func distinctOps(v []Violation) int {
	ops := make(map[string]bool, len(v))
	for _, x := range v {
		ops[x.Op] = true
	}
	return len(ops)
}

// TestNullSemantics_SensitivityToWrongAgeBookkeeping proves the battery FIRES
// when the oracle's age-present bookkeeping is perturbed (the in-package test
// seam): a single flipped person shifts the count/IS NULL/aggregate/3VL/
// OPTIONAL MATCH references at once, so MULTIPLE probes must flag the
// disagreement with the untouched engine.
func TestNullSemantics_SensitivityToWrongAgeBookkeeping(t *testing.T) {
	t.Run("age dropped from an age-bearing person", func(t *testing.T) {
		a, o := buildNullSemanticsFixture(t)
		delete(o.nodes[o.byName["a2"]].Properties, "age")
		v := CheckNullSemantics(0, o, a)
		if len(v) == 0 {
			t.Fatal("battery FAILED to fire on an age dropped from the model")
		}
		if n := distinctOps(v); n < 2 {
			t.Fatalf("only %d distinct probe(s) fired; a flipped age partition must trip multiple probes: %v", n, v)
		}
	})
	t.Run("phantom age on an ageless person", func(t *testing.T) {
		a, o := buildNullSemanticsFixture(t)
		o.nodes[o.byName["n3"]].Properties["age"] = int64(99)
		v := CheckNullSemantics(0, o, a)
		if len(v) == 0 {
			t.Fatal("battery FAILED to fire on a phantom age in the model")
		}
		if n := distinctOps(v); n < 2 {
			t.Fatalf("only %d distinct probe(s) fired; a phantom age must trip multiple probes: %v", n, v)
		}
	})
}

// TestNullSemantics_NonVacuityFires proves the terminal non-vacuity assertion
// rejects a run whose NULL shapes were degenerate.
func TestNullSemantics_NonVacuityFires(t *testing.T) {
	t.Run("no ageless person", func(t *testing.T) {
		o := NewGraphOracle()
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "x", "age": int64(20)})
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "y", "age": int64(60)})
		if v := checkNullSemanticsNonVacuity(0, o); len(v) == 0 {
			t.Fatal("non-vacuity FAILED to fire with an empty IS NULL population")
		}
	})
	t.Run("one-sided OPTIONAL MATCH shapes", func(t *testing.T) {
		o := NewGraphOracle()
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "x", "age": int64(20)})
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "y", "age": int64(60)})
		// Two ageless Persons, NEITHER with an outgoing KNOWS edge: the
		// OPTIONAL MATCH probe never saw a matched (non-NULL) row.
		o.ApplyCreate(tmplCreatePersonNoAge, map[string]any{"name": "p", "city": "c0"})
		o.ApplyCreate(tmplCreatePersonNoAge, map[string]any{"name": "q", "city": "c1"})
		if v := checkNullSemanticsNonVacuity(0, o); len(v) == 0 {
			t.Fatal("non-vacuity FAILED to fire with one-sided OPTIONAL MATCH shapes")
		}
	})
}

// TestNullSemantics_Scenario_Passes runs the registered null-semantics
// scenario end to end (periodic, post-crash-recovery, and terminal battery,
// per-op counters oracle, and the terminal non-vacuity assertion) at its
// default seed.
func TestNullSemantics_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioNullSemantics)
	if !ok {
		t.Fatalf("null-semantics scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("null-semantics run: %v", err)
	}
	if report != nil {
		t.Fatalf("null-semantics reported a violation (a NULL/3VL probe disagreed with the oracle partition):\n%s", report)
	}
}
