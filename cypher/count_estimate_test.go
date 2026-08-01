package cypher

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
)

// resolverFor builds the production count-estimate source over an engine's graph
// and count store, exactly as the read-path build threads it (api.go:1799).
func resolverFor(eng *Engine) *lpgLabelResolver {
	return &lpgLabelResolver{g: eng.g.ReadAt(nil), eng: eng}
}

// TestCountEstimate_ExactCounts confirms the provider returns the exact E / D / T
// counts as estExact on a clean (pure-CREATE) store.
func TestCountEstimate_ExactCounts(t *testing.T) {
	eng, _ := newCountEngine(t, 0)
	// (:A)-[:R]->(:B) twice, and (:A)-[:R]->(:C) once.
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	mustRun(t, eng, "CREATE (:A)-[:R]->(:C)")
	src := resolverFor(eng)

	// E(R) = 3, exact.
	if e := relCardinalityEstimate(src, "R"); e.source != estExact || e.rows != 3 {
		t.Errorf("E(R) = %+v, want {3 exact}", e)
	}
	// D(A,R,OUT) = 3 (three R edges out of A-labelled nodes), exact.
	if e := degreeCardinalityEstimate(src, "A", "R", count.Out); e.source != estExact || e.rows != 3 {
		t.Errorf("D(A,R,OUT) = %+v, want {3 exact}", e)
	}
	// D(B,R,IN) = 2, exact.
	if e := degreeCardinalityEstimate(src, "B", "R", count.In); e.source != estExact || e.rows != 2 {
		t.Errorf("D(B,R,IN) = %+v, want {2 exact}", e)
	}
	// T(A,R,B) = 2, exact.
	if e := tripleCardinalityEstimate(src, "A", "R", "B"); e.source != estExact || e.rows != 2 {
		t.Errorf("T(A,R,B) = %+v, want {2 exact}", e)
	}
	// T(A,R,C) = 1, exact.
	if e := tripleCardinalityEstimate(src, "A", "R", "C"); e.source != estExact || e.rows != 1 {
		t.Errorf("T(A,R,C) = %+v, want {1 exact}", e)
	}
	// The provider must never veto on a clean store (drives a real P3 decision).
	if planStaysDefault(
		relCardinalityEstimate(src, "R"),
		degreeCardinalityEstimate(src, "A", "R", count.Out),
		tripleCardinalityEstimate(src, "A", "R", "B"),
	) {
		t.Errorf("clean-store estimates must not veto the plan")
	}
}

// TestCountEstimate_UnknownIsExactZero confirms an unknown label or type yields
// an exact 0 (mirroring ResolveLabelCount), never a fallback.
func TestCountEstimate_UnknownIsExactZero(t *testing.T) {
	eng, _ := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	src := resolverFor(eng)

	if e := relCardinalityEstimate(src, "NOSUCHTYPE"); e.source != estExact || e.rows != 0 {
		t.Errorf("E(unknown) = %+v, want {0 exact}", e)
	}
	if e := degreeCardinalityEstimate(src, "NOLABEL", "R", count.Out); e.source != estExact || e.rows != 0 {
		t.Errorf("D(unknown-label,...) = %+v, want {0 exact}", e)
	}
	if e := tripleCardinalityEstimate(src, "A", "NOSUCHTYPE", "B"); e.source != estExact || e.rows != 0 {
		t.Errorf("T(A,unknown,B) = %+v, want {0 exact}", e)
	}
}

// TestCountEstimate_DirtyVetoes confirms a relabel-dirtied D/T family yields
// estFallback (an absolute veto) while E and the still-exact families stay exact.
func TestCountEstimate_DirtyVetoes(t *testing.T) {
	eng, _ := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	// Relabel the source: dirties D(X,*,IN) and T(*,*,X); keeps D(X,*,OUT) exact.
	mustRun(t, eng, "MATCH (a:A) SET a:X")
	src := resolverFor(eng)

	// E is never dirty.
	if e := relCardinalityEstimate(src, "R"); e.source != estExact || e.rows != 1 {
		t.Errorf("E(R) = %+v, want {1 exact}", e)
	}
	// D(X,R,IN) is dirty -> fallback (an absolute veto).
	if e := degreeCardinalityEstimate(src, "X", "R", count.In); e.source != estFallback {
		t.Errorf("D(X,R,IN) = %+v, want fallback (dirty)", e)
	}
	if !planStaysDefault(degreeCardinalityEstimate(src, "X", "R", count.In)) {
		t.Errorf("a dirty D-IN estimate must veto the plan")
	}
	// D(X,R,OUT) stays exact (OUT-exact relabel).
	if e := degreeCardinalityEstimate(src, "X", "R", count.Out); e.source != estExact || e.rows != 1 {
		t.Errorf("D(X,R,OUT) = %+v, want {1 exact}", e)
	}
	// T with X in the b-position is dirty -> fallback.
	if e := tripleCardinalityEstimate(src, "A", "R", "X"); e.source != estFallback {
		t.Errorf("T(A,R,X) = %+v, want fallback (X dirty in b-position)", e)
	}
	// T with X in the a-position stays exact (a-position not dirtied by a source relabel).
	if e := tripleCardinalityEstimate(src, "X", "R", "B"); e.source != estExact || e.rows != 1 {
		t.Errorf("T(X,R,B) = %+v, want {1 exact}", e)
	}
	// The pre-existing exact triple is unaffected.
	if e := tripleCardinalityEstimate(src, "A", "R", "B"); e.source != estExact || e.rows != 1 {
		t.Errorf("T(A,R,B) = %+v, want {1 exact}", e)
	}
}

// TestCountEstimate_NilStoreFallsBack confirms a resolver with no count store
// (no engine) falls back (so the trustworthiness veto keeps the default plan).
func TestCountEstimate_NilStoreFallsBack(t *testing.T) {
	src := &lpgLabelResolver{g: nil, eng: nil}
	if e := relCardinalityEstimate(src, "R"); e.source != estFallback {
		t.Errorf("E with nil store = %+v, want fallback", e)
	}
	if e := degreeCardinalityEstimate(src, "A", "R", count.Out); e.source != estFallback {
		t.Errorf("D with nil store = %+v, want fallback", e)
	}
	if e := tripleCardinalityEstimate(src, "A", "R", "B"); e.source != estFallback {
		t.Errorf("T with nil store = %+v, want fallback", e)
	}
	// A nil source also falls back.
	if e := relCardinalityEstimate(nil, "R"); e.source != estFallback {
		t.Errorf("E with nil source = %+v, want fallback", e)
	}
}
