package sim

import (
	"context"
	"testing"
)

// TestExprLiterals_AllPass verifies every graph-independent expression probe
// (UNION/CASE/comprehension/reduce/quantifier/function/subscript/slice/map/
// temporal) evaluates to its known-constant expectation on a fresh engine.
func TestExprLiterals_AllPass(t *testing.T) {
	if len(exprLiteralCases) < 30 {
		t.Fatalf("expr-literal battery is thin: %d cases", len(exprLiteralCases))
	}
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	if v := CheckExprLiterals(0, a); len(v) > 0 {
		t.Fatalf("expr-literal battery reported violations: %v", v)
	}
}

// seedSurfaceGraph creates n Persons named s0..s(n-1) with ages and a couple of
// KNOWS edges in BOTH the engine and the oracle, so the surface-extended checker
// sees a consistent state.
func seedSurfaceGraph(t *testing.T, a *EngineAdapter, o *GraphOracle, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		p := map[string]any{"name": fmtName(i), "age": int64(10 * i)}
		if _, err := a.RunWrite(ctx, tmplCreatePerson, p); err != nil {
			t.Fatalf("create %s: %v", fmtName(i), err)
		}
		o.ApplyCreate(tmplCreatePerson, p)
	}
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		if e[0] >= n || e[1] >= n {
			continue
		}
		p := map[string]any{"a": fmtName(e[0]), "b": fmtName(e[1])}
		if _, err := a.RunWrite(ctx, tmplCreateKnows, p); err != nil {
			t.Fatalf("knows: %v", err)
		}
		o.ApplyCreate(tmplCreateKnows, p)
	}
}

func fmtName(i int) string { return "s" + itoaSmall(i) }
func itoaSmall(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestCypherSurfaceExtended_PassAndCatch verifies the extended checker is clean
// on a consistent Person/KNOWS graph and CATCHES an injected oracle divergence.
func TestCypherSurfaceExtended_PassAndCatch(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	o := NewGraphOracle()
	seedSurfaceGraph(t, a, o, 6)

	if v := CheckCypherSurfaceExtended(0, o, a); len(v) > 0 {
		t.Fatalf("consistent graph should be clean, got: %v", v)
	}

	// Inject a divergence: tell the oracle a Person exists that the engine lacks,
	// so the EXISTS/count/aggregate invariants no longer match.
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(999)})
	if v := CheckCypherSurfaceExtended(0, o, a); len(v) == 0 {
		t.Fatalf("checker missed an injected oracle divergence")
	}
}
