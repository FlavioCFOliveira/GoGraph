package cypher_test

// set_map_function_expr_test.go — regression coverage for whole-entity SET whose
// RHS is a map-returning EXPRESSION rather than a bound variable (#2027).
//
// `SET n = properties(m)` (and other map-returning expressions such as a map
// projection) used to fail at build time — the translator stringified the
// non-map-literal expression and the exec literal-map parser rejected it with
// "expected map literal enclosed in {}". openCypher permits any expression that
// yields a map on the RHS; it must be evaluated per row and applied as a
// whole-entity map. The capability is delivered by the FromExpr path.

import "testing"

// TestSetExpr_PropertiesFunc_Replace: `SET n = properties(m)` copies m's
// properties (replace clears n's others). This is the exact #2027 repro.
func TestSetExpr_PropertiesFunc_Replace(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1, keep:'old'}), (:M {x:'V', y:'W'})`)
	drainRunInTx(t, eng, `MATCH (n:N),(m:M) SET n = properties(m)`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.y AS v`); got != "W" {
		t.Fatalf("n.y = %q, want \"W\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.keep AS v`) {
		t.Fatal("n.keep must be cleared by = replace")
	}
}

// TestSetExpr_PropertiesFunc_Merge: `SET n += properties(m)` merges m's
// properties, keeping n's other properties.
func TestSetExpr_PropertiesFunc_Merge(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1}), (:M {x:'V'})`)
	drainRunInTx(t, eng, `MATCH (n:N),(m:M) SET n += properties(m)`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be kept by += merge")
	}
}

// TestSetExpr_MapProjection: `SET n += m{.x}` writes the projected entries.
func TestSetExpr_MapProjection(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1}), (:M {x:'V', y:'W'})`)
	drainRunInTx(t, eng, `MATCH (n:N),(m:M) SET n += m{.x}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	// .y was not projected → not written.
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.y AS v`) {
		t.Fatal("n.y must not be written (not in the projection)")
	}
}

// TestSetExpr_PropertiesFunc_NoBuildError guards that the build no longer
// rejects the map-returning expression form (the #2027 symptom was a hard
// build-time error).
func TestSetExpr_PropertiesFunc_NoBuildError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1}), (:M {x:'V'})`)
	if err := runDrainErr(t, eng, `MATCH (n:N),(m:M) SET n = properties(m)`); err != nil {
		t.Fatalf("SET n = properties(m) must not error: %v", err)
	}
}
