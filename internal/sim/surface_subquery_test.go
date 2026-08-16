package sim

import (
	"context"
	"testing"
)

// seedFilteredSubqueryGraph builds a Person/KNOWS graph chosen so that EVERY
// expectation the filtered-subquery probes assert is non-degenerate:
//
//	s0 age 10   s1 age 20   s2 age 30   s3 age 40   s4 age 50   s5 age 60
//	s0 -KNOWS-> s1     s0 -KNOWS-> s4
//	s1 -KNOWS-> s4     s2 -KNOWS-> s0
//
// The median age is 40, so exactly two of the four edges land on a Person that
// old or older, and exactly two distinct Persons have such an edge. The smallest
// KNOWS target name is s0, which exactly one Person knows. Every probe value is
// therefore strictly between 0 and the unfiltered total — which is what makes the
// probes able to distinguish "the filter worked" from both "nothing matched" and
// "the filter was ignored".
func seedFilteredSubqueryGraph(t *testing.T, a *EngineAdapter, o *GraphOracle) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		p := map[string]any{"name": fmtName(i), "age": int64(10 * (i + 1))}
		if _, err := a.RunWrite(ctx, tmplCreatePerson, p); err != nil {
			t.Fatalf("create %s: %v", fmtName(i), err)
		}
		o.ApplyCreate(tmplCreatePerson, p)
	}
	for _, e := range [][2]int{{0, 1}, {0, 4}, {1, 4}, {2, 0}} {
		p := map[string]any{"a": fmtName(e[0]), "b": fmtName(e[1])}
		if _, err := a.RunWrite(ctx, tmplCreateKnows, p); err != nil {
			t.Fatalf("knows %v: %v", e, err)
		}
		o.ApplyCreate(tmplCreateKnows, p)
	}
}

// TestSubqueryFilterStats_NonDegenerate pins the oracle reference the filtered
// probes assert against, and — more importantly — pins that it is NOT degenerate.
//
// A probe whose expectation equals zero, or equals the unfiltered total, cannot
// tell a working filter from a broken one. Asserting the expectations here is what
// stops [checkFilteredSubqueries] silently decaying into that state if the seed
// graph is ever changed.
func TestSubqueryFilterStats_NonDegenerate(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	o := NewGraphOracle()
	seedFilteredSubqueryGraph(t, a, o)

	ages := o.personAges()
	st := o.subqueryFilterStats(ages[len(ages)/2])
	if !st.Usable {
		t.Fatalf("seed graph produced an unusable reference: %+v", st)
	}
	if st.AgeFloor != 40 {
		t.Errorf("AgeFloor: got %d, want 40 (the median of %v)", st.AgeFloor, ages)
	}
	if st.TargetName != "s0" {
		t.Errorf("TargetName: got %q, want %q", st.TargetName, "s0")
	}
	if st.PersonToPerson != 4 {
		t.Errorf("PersonToPerson: got %d, want 4", st.PersonToPerson)
	}
	// Each filtered expectation must be strictly inside (0, PersonToPerson).
	for _, c := range []struct {
		name string
		got  int64
	}{
		{"ToNamed", st.ToNamed},
		{"SrcToNamed", st.SrcToNamed},
		{"ToAgeAtLeast", st.ToAgeAtLeast},
		{"SrcWithAgeAtLeast", st.SrcWithAgeAtLeast},
	} {
		if c.got <= 0 {
			t.Errorf("%s = %d: a zero expectation cannot distinguish a working filter from a broken one", c.name, c.got)
		}
		if c.got >= st.PersonToPerson {
			t.Errorf("%s = %d: equals or exceeds the unfiltered total %d, so an IGNORED filter would still pass",
				c.name, c.got, st.PersonToPerson)
		}
	}
}

// TestCypherSurfaceExtended_FilteredSubqueries_PassAndCatch is the pass-and-catch
// meta-test for the #2507 probes: the extended checker must be clean on a
// consistent graph, and at least one of the FILTERED subquery probes must fire
// when the model and the engine disagree about an edge those probes read.
//
// Naming the probes explicitly is the point. A run that merely reports "some
// violation" would also pass if the filtered probes had never executed and an
// unrelated aggregate caught the divergence instead — which is exactly the failure
// mode #2507 escaped through.
func TestCypherSurfaceExtended_FilteredSubqueries_PassAndCatch(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	o := NewGraphOracle()
	seedFilteredSubqueryGraph(t, a, o)

	if v := CheckCypherSurfaceExtended(0, o, a); len(v) > 0 {
		t.Fatalf("consistent graph should be clean, got: %v", v)
	}

	// Inject an ORACLE-only KNOWS edge landing on the probe's target Person, so
	// every expectation that reads edges into s0 moves by one while the engine's
	// answer stays where it was.
	o.ApplyCreate(tmplCreateKnows, map[string]any{"a": fmtName(3), "b": "s0"})

	vs := CheckCypherSurfaceExtended(0, o, a)
	if len(vs) == 0 {
		t.Fatalf("checker missed an injected oracle divergence")
	}
	filtered := map[string]bool{
		"COUNT subquery inline filter":    true,
		"COUNT subquery WHERE filter":     true,
		"pattern predicate inline filter": true,
	}
	var fired []string
	for _, v := range vs {
		if filtered[v.Op] {
			fired = append(fired, v.Op)
		}
	}
	if len(fired) == 0 {
		t.Fatalf("no FILTERED subquery probe fired; the divergence was caught only by:\n%v", vs)
	}
}

// TestCypherSurfaceExtended_FilteredSubqueries_SkipWhenVacuous asserts the probes
// are SKIPPED, not reported as passing, on a graph that holds no Person→Person
// KNOWS edge — the state in which every expectation would be zero and no probe
// could fail.
func TestCypherSurfaceExtended_FilteredSubqueries_SkipWhenVacuous(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	o := NewGraphOracle()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		p := map[string]any{"name": fmtName(i), "age": int64(10 * (i + 1))}
		if _, err := a.RunWrite(ctx, tmplCreatePerson, p); err != nil {
			t.Fatalf("create %s: %v", fmtName(i), err)
		}
		o.ApplyCreate(tmplCreatePerson, p)
	}
	st := o.subqueryFilterStats(20)
	if st.Usable {
		t.Fatalf("edgeless graph reported a usable reference: %+v", st)
	}
	if v := CheckCypherSurfaceExtended(0, o, a); len(v) > 0 {
		t.Fatalf("edgeless graph should be clean, got: %v", v)
	}
}
